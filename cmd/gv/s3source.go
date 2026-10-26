package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/semaphore"
)

const (
	// s3RetryMaxAttempts bounds one request's attempts, including the first.
	s3RetryMaxAttempts = 10
	// s3RetryMaxBackoff caps the exponential delay between attempts.
	s3RetryMaxBackoff = 20 * time.Second
	// s3Listers bounds how many listings an S3 walk runs at once. A listing
	// returns at most a thousand keys and its pages are sequential, so a wide
	// tree is enumerated quickly only by listing sibling prefixes together.
	s3Listers = 32
	// s3ListAhead bounds how many prefix listings the walk holds beyond the
	// entry its depth-first consumer is on, as walkReadAhead does locally.
	s3ListAhead = 4 * s3Listers
	// s3Readers is how many objects an upload reads at once. A GET of a small
	// object is all latency, tens of milliseconds even from inside its region,
	// so the rate of a source of small files is this many objects per GET.
	s3Readers = 256
	// s3ReadWindow bounds how many objects the readers run ahead of the
	// oldest one the encoder still waits for, so one slow GET holds up the
	// encoder but not the readers behind it.
	s3ReadWindow = 4 * uploadBatchFiles
	// s3ReadBytes bounds the content read ahead of the encoder. The chunk
	// being filled holds up to maxBatchContent of it, so the bound must leave
	// room for at least one more object of that size beside it.
	s3ReadBytes = 4 * maxBatchContent
)

// s3API is the part of the S3 client an upload uses.
type s3API interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// s3Location is a parsed s3://BUCKET/KEY source.
type s3Location struct {
	bucket string
	key    string
}

func (l s3Location) String() string { return "s3://" + l.bucket + "/" + l.key }

// parseS3URI reports whether source names S3 rather than a local path. The
// key is taken verbatim, since S3 keys may hold any character a URL escapes.
func parseS3URI(source string) (s3Location, bool, error) {
	rest, ok := strings.CutPrefix(source, "s3://")
	if !ok {
		return s3Location{}, false, nil
	}
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return s3Location{}, true, errors.New("source must be s3://BUCKET or s3://BUCKET/KEY")
	}
	return s3Location{bucket: bucket, key: key}, true, nil
}

// s3Client returns the client an upload reads bucket through.
func (a *app) s3Client(ctx context.Context, bucket string) (s3API, error) {
	if a.openS3 != nil {
		return a.openS3(ctx, bucket)
	}
	return newS3Client(ctx, bucket)
}

// newS3Client builds a client from the default AWS configuration chain, with
// a connection pool for every concurrent read and newS3Retryer's policy,
// which backs off on SlowDown instead of failing a long upload.
//
// The region is the bucket's own. A configured region is only a starting
// point: S3 refuses a request signed for another region, and a source bucket
// is often somewhere other than the caller's default.
func newS3Client(ctx context.Context, bucket string) (s3API, error) {
	connections := s3Readers + 2*s3Listers
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(transport *http.Transport) {
		transport.MaxIdleConns = connections
		transport.MaxIdleConnsPerHost = connections
		// A GET that never answers would hold the ordered pipeline behind it
		// forever; a timeout turns it into a retry.
		transport.ResponseHeaderTimeout = 30 * time.Second
	})
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithHTTPClient(httpClient), awsconfig.WithRetryer(newS3Retryer))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if awsConfig.Region == "" {
		awsConfig.Region = "us-east-1"
	}
	client := s3.NewFromConfig(awsConfig)
	if isDirectoryBucket(bucket) {
		// A directory bucket's endpoint is derived from its zone, so a wrong
		// region fails to resolve rather than redirect; there is nothing to
		// discover.
		return client, nil
	}
	if region := bucketRegion(ctx, client, bucket); region != "" && region != awsConfig.Region {
		client = s3.NewFromConfig(awsConfig, func(options *s3.Options) { options.Region = region })
	}
	return client, nil
}

// bucketRegion asks S3 where bucket is. S3 names the region in a header of
// its answer to HeadBucket, including the redirect it sends when the request
// went to the wrong region and the refusal it sends to a caller that may not
// list the bucket. It returns "" when the answer does not say.
func bucketRegion(ctx context.Context, client *s3.Client, bucket string) string {
	output, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}, func(options *s3.Options) {
		options.Retryer = aws.NopRetryer{}
	})
	if err == nil {
		return aws.ToString(output.BucketRegion)
	}
	var response *awshttp.ResponseError
	if errors.As(err, &response) && response.Response != nil {
		return response.Response.Header.Get("X-Amz-Bucket-Region")
	}
	return ""
}

// s3Source is the bucket an upload reads, and either the one object it
// uploads or the prefix it uploads everything under.
type s3Source struct {
	client s3API
	bucket string
	// prefix is the directory a recursive source uploads: "" for the whole
	// bucket, or a key prefix ending in "/".
	prefix string
	// object is the single object a source names, or nil for a directory.
	object *s3Object
	size   int64
	mtime  time.Time
	// unordered is set for a directory bucket, which lists a prefix in no
	// particular order; its listings are sorted before they are walked.
	unordered bool
}

// s3Object is one object an upload reads. The ETag is the version the
// listing saw: every read asks for that version, so a document's content
// always matches the size and modification time it is published with, and a
// second upload of an unchanged source produces the same requests.
type s3Object struct {
	source *s3Source
	key    string
	etag   string
}

func (o *s3Object) uri() string { return "s3://" + o.source.bucket + "/" + o.key }

// open starts reading the object.
func (o *s3Object) open(ctx context.Context) (io.ReadCloser, error) {
	input := &s3.GetObjectInput{Bucket: aws.String(o.source.bucket), Key: aws.String(o.key)}
	if o.etag != "" {
		input.IfMatch = aws.String(o.etag)
	}
	output, err := o.source.client.GetObject(ctx, input)
	if err != nil {
		return nil, o.readError(err)
	}
	return output.Body, nil
}

// errArchived marks an object S3 will not serve until it is restored.
var errArchived = errors.New("it is archived; restore it to upload it")

func (o *s3Object) readError(err error) error {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiErr.ErrorCode() {
		case "PreconditionFailed":
			return fmt.Errorf("%s changed during the upload; upload it again", o.uri())
		case "NoSuchKey", "NotFound":
			return fmt.Errorf("%s was deleted during the upload", o.uri())
		case "InvalidObjectState":
			return fmt.Errorf("read %s: %w", o.uri(), errArchived)
		}
	}
	return fmt.Errorf("read %s: %w", o.uri(), err)
}

// resolveS3Source decides whether location is one object or a directory, the
// way a local path is a file or a directory. A key that ends in "/", or no
// key at all, is a directory. Otherwise a recursive upload prefers the
// directory of that name, as `aws s3 cp --recursive` does, and a plain upload
// prefers the object.
func resolveS3Source(ctx context.Context, client s3API, location s3Location, recursive bool) (*s3Source, error) {
	source := &s3Source{client: client, bucket: location.bucket, unordered: isDirectoryBucket(location.bucket)}
	key := location.key
	if key == "" || strings.HasSuffix(key, "/") {
		if !recursive {
			return nil, errors.New("source is a directory; use --recursive")
		}
		source.prefix = key
		return source, nil
	}
	if recursive {
		found, err := source.hasEntries(ctx, key+"/")
		if err != nil {
			return nil, err
		}
		if found {
			source.prefix = key + "/"
			return source, nil
		}
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(location.bucket), Key: aws.String(key)})
	if err == nil {
		source.object = &s3Object{source: source, key: key, etag: aws.ToString(head.ETag)}
		source.size, source.mtime = aws.ToInt64(head.ContentLength), aws.ToTime(head.LastModified)
		return source, nil
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotFound" && apiErr.ErrorCode() != "NoSuchKey" {
		return nil, fmt.Errorf("inspect source: %w", err)
	}
	if !recursive {
		found, err := source.hasEntries(ctx, key+"/")
		if err != nil {
			return nil, err
		}
		if found {
			return nil, errors.New("source is a directory; use --recursive")
		}
	}
	return nil, fmt.Errorf("inspect source: %s does not exist", location)
}

// hasEntries reports whether anything is stored under prefix.
func (s *s3Source) hasEntries(ctx context.Context, prefix string) (bool, error) {
	output, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), Delimiter: aws.String("/"), MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("list s3://%s/%s: %w", s.bucket, prefix, err)
	}
	return len(output.Contents)+len(output.CommonPrefixes) > 0, nil
}

// walk calls emit for every object an upload of the source publishes, in key
// order, and returns how many there are. A nil emit only counts them, which
// is the pass that gives the progress line its total. skip, which may be nil,
// hears about each object the upload leaves out; seen, which may be nil,
// hears the running count.
func (s *s3Source) walk(ctx context.Context, remotePrefix string, emit func(uploadFile) error, skip func(string), seen func(files int64)) (int64, error) {
	if s.object != nil {
		if emit == nil {
			return 1, nil
		}
		key, err := remoteKey(remotePrefix, path.Base(s.object.key))
		if err != nil {
			return 0, fmt.Errorf("map %s: %w", s.object.uri(), err)
		}
		return 1, emit(uploadFile{key: key, size: s.size, mtime: s.mtime, object: s.object})
	}
	ctx, cancel := context.WithCancel(ctx)
	walker := &s3Walker{
		ctx: ctx, source: s, remotePrefix: remotePrefix, emit: emit, skip: skip, seen: seen,
		tasks: make(chan func(), s3ListAhead),
	}
	for range s3Listers {
		walker.workers.Go(func() {
			for task := range walker.tasks {
				task()
			}
		})
	}
	defer func() {
		// Listings read ahead of a walk that ended early are abandoned
		// rather than finished.
		cancel()
		close(walker.tasks)
		walker.workers.Wait()
	}()
	if err := walker.walkPrefix(s.prefix, walker.list(s.prefix, nil)); err != nil {
		return walker.files.Load(), err
	}
	walker.report()
	return walker.files.Load(), nil
}

// s3Walker streams the objects under a prefix in key order, listing the
// prefixes it will descend into before it reaches them, as sourceWalker does
// for a local directory.
//
// It walks with "/" as the delimiter, one prefix at a time, rather than
// paging through one flat listing: pages of a listing are sequential, but
// sibling prefixes can be listed at once, which is what lets a tree of
// millions of objects be enumerated at the rate the upload reads it. A depth-
// first walk of prefixes in key order visits keys in the same order a flat
// listing would.
type s3Walker struct {
	ctx          context.Context
	source       *s3Source
	remotePrefix string
	emit         func(uploadFile) error
	skip         func(string)
	seen         func(files int64)

	tasks       chan func()
	workers     sync.WaitGroup
	outstanding atomic.Int64
	files       atomic.Int64
}

// s3Entry is one object or one common prefix, which ends in "/", of a listing.
type s3Entry struct {
	key      string
	prefix   bool
	size     int64
	mtime    time.Time
	etag     string
	archived bool
}

// s3Page is one page of a prefix's listing, in key order. token continues
// the listing, or is nil on its last page.
type s3Page struct {
	entries []s3Entry
	token   *string
	err     error
}

func (w *s3Walker) report() {
	if w.seen != nil {
		w.seen(w.files.Load())
	}
}

// list reads one page of the listing of prefix, or the whole listing of a
// directory bucket, whose pages follow no order and are sorted here.
func (w *s3Walker) list(prefix string, token *string) s3Page {
	var page s3Page
	for {
		input := &s3.ListObjectsV2Input{
			Bucket: aws.String(w.source.bucket), Prefix: aws.String(prefix), Delimiter: aws.String("/"), ContinuationToken: token,
		}
		if !w.source.unordered {
			// Only general purpose buckets archive objects, and only they
			// report whether an archived object has been restored.
			input.OptionalObjectAttributes = []types.OptionalObjectAttributes{types.OptionalObjectAttributesRestoreStatus}
		}
		output, err := w.source.client.ListObjectsV2(w.ctx, input)
		if err != nil {
			return s3Page{err: fmt.Errorf("list s3://%s/%s: %w", w.source.bucket, prefix, err)}
		}
		for _, common := range output.CommonPrefixes {
			page.entries = append(page.entries, s3Entry{key: aws.ToString(common.Prefix), prefix: true})
		}
		for _, object := range output.Contents {
			page.entries = append(page.entries, s3Entry{
				key: aws.ToString(object.Key), size: aws.ToInt64(object.Size), mtime: aws.ToTime(object.LastModified),
				etag: aws.ToString(object.ETag), archived: archived(object),
			})
		}
		token = nil
		if aws.ToBool(output.IsTruncated) {
			token = output.NextContinuationToken
		}
		if token == nil || !w.source.unordered {
			break
		}
	}
	// Objects and prefixes come back as two lists. Merging them by key puts
	// each prefix where a flat listing would have put its keys.
	sort.Slice(page.entries, func(i, j int) bool { return page.entries[i].key < page.entries[j].key })
	page.token = token
	return page
}

// archived reports whether S3 will refuse to serve object until it is
// restored.
func archived(object types.Object) bool {
	switch object.StorageClass {
	case types.ObjectStorageClassGlacier, types.ObjectStorageClassDeepArchive:
	default:
		return false
	}
	restore := object.RestoreStatus
	return restore == nil || aws.ToBool(restore.IsRestoreInProgress) || restore.RestoreExpiryDate == nil
}

// walkPrefix consumes the listing of prefix, whose first page is given, and
// descends into each prefix it holds at its place in key order.
func (w *s3Walker) walkPrefix(prefix string, page s3Page) error {
	for {
		if page.err != nil {
			return page.err
		}
		if err := w.ctx.Err(); err != nil {
			return err
		}
		// The next page is requested before this one is walked, so it is
		// there by the time the walk is done with this one.
		var next chan s3Page
		if page.token != nil {
			next = make(chan s3Page, 1)
			token := page.token
			go func() { next <- w.list(prefix, token) }()
		}
		kept := page.entries[:0]
		subprefixes := make([]string, 0, 8)
		files := 0
		for _, entry := range page.entries {
			name := strings.TrimSuffix(entry.key[len(prefix):], "/")
			switch {
			case name == "" && !entry.prefix:
				// The marker a console writes for an empty folder.
				continue
			case name == ".git":
				continue
			case name == "" || name == "." || name == "..":
				w.skipped(entry.key, "its path has an empty, dot, or dot-dot component")
				continue
			case entry.archived:
				w.skipped(entry.key, errArchived.Error())
				continue
			case entry.prefix:
				subprefixes = append(subprefixes, entry.key)
			default:
				files++
			}
			kept = append(kept, entry)
		}
		w.files.Add(int64(files))
		w.report()
		ahead := w.readAhead(subprefixes)
		descended := 0
		for _, entry := range kept {
			if entry.prefix {
				if err := w.walkPrefix(entry.key, ahead.take(descended)); err != nil {
					return err
				}
				descended++
				w.report()
				continue
			}
			if w.emit == nil {
				continue
			}
			object := &s3Object{source: w.source, key: entry.key, etag: entry.etag}
			key, err := remoteKey(w.remotePrefix, entry.key[len(w.source.prefix):])
			if err != nil {
				return fmt.Errorf("map %s: %w", object.uri(), err)
			}
			if err := w.emit(uploadFile{key: key, size: entry.size, mtime: entry.mtime, object: object}); err != nil {
				return err
			}
		}
		if next == nil {
			return nil
		}
		page = <-next
	}
}

func (w *s3Walker) skipped(key, reason string) {
	if w.skip != nil && w.emit != nil {
		w.skip(fmt.Sprintf("skipping s3://%s/%s: %s", w.source.bucket, key, reason))
	}
}

// s3ReadAhead lists prefixes before the depth-first walk reaches them and
// hands their first pages back in the order the walk asks for them.
type s3ReadAhead struct {
	walk     *s3Walker
	prefixes []string
	results  []chan s3Page
	started  int
}

func (w *s3Walker) readAhead(prefixes []string) *s3ReadAhead {
	ahead := &s3ReadAhead{walk: w, prefixes: prefixes, results: make([]chan s3Page, len(prefixes))}
	ahead.fill()
	return ahead
}

func (r *s3ReadAhead) fill() {
	for r.started < len(r.prefixes) && r.walk.outstanding.Load() < s3ListAhead {
		index := r.started
		r.started++
		r.walk.outstanding.Add(1)
		result := make(chan s3Page, 1)
		r.results[index] = result
		prefix := r.prefixes[index]
		r.walk.tasks <- func() { result <- r.walk.list(prefix, nil) }
	}
}

func (r *s3ReadAhead) take(index int) s3Page {
	result := r.results[index]
	if result == nil {
		// The walk already holds its share of listings ahead; list this one
		// here, as the local walk does.
		if index >= r.started {
			r.started = index + 1
		}
		return r.walk.list(r.prefixes[index], nil)
	}
	r.results[index] = nil
	page := <-result
	r.walk.outstanding.Add(-1)
	r.fill()
	return page
}

// s3Read is one object being read for the encoder.
type s3Read struct {
	file   uploadFile
	done   chan struct{}
	result readResult
	// held is the share of the read-ahead budget its content holds.
	held int64
}

// encodeUploads turns the named objects of an S3 source into request bodies,
// as the package-level encodeUploads does for local files. The listing
// already measured every object, so nothing is opened to learn its size: a
// pool of readers fetches objects far ahead of the encoder, which takes them
// in the order they were named and cuts batches by the same rules as a local
// upload.
//
// A local upload reads one chunk at a time because a read costs microseconds.
// A GET costs tens of milliseconds, so the readers never wait for a chunk to
// finish: they run ahead by up to s3ReadWindow objects and s3ReadBytes of
// content, and a slow object delays only the batch it belongs to.
func (s *s3Source) encodeUploads(ctx context.Context, names <-chan uploadFile, recursive bool, warn func(string), consume func(uploadJob) error) error {
	var group sync.WaitGroup
	defer group.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	budget := semaphore.NewWeighted(s3ReadBytes)
	reads := make(chan *s3Read, s3ReadWindow)
	work := make(chan *s3Read, s3Readers)
	for range s3Readers {
		group.Go(func() {
			for read := range work {
				read.result = readS3Object(ctx, read.file)
				close(read.done)
			}
		})
	}
	group.Go(func() {
		defer close(reads)
		defer close(work)
		for {
			var file uploadFile
			select {
			case next, ok := <-names:
				if !ok {
					return
				}
				file = next
			case <-ctx.Done():
				return
			}
			read := &s3Read{file: file, done: make(chan struct{})}
			fetch := false
			switch {
			case file.size > maxDocumentSize:
				read.result = readResult{file: file, oversize: true}
			case file.size > maxBatchContent:
				// Streamed from S3 by the request that publishes it.
				read.result = readResult{file: file, large: true}
			default:
				if budget.Acquire(ctx, file.size) != nil {
					return
				}
				read.held, fetch = file.size, true
			}
			if !fetch {
				close(read.done)
			}
			select {
			case reads <- read:
			case <-ctx.Done():
				return
			}
			if fetch {
				select {
				case work <- read:
				case <-ctx.Done():
					return
				}
			}
		}
	})

	chunk := make([]readResult, 0, uploadBatchFiles)
	var reserved, held int64
	flush := func() error {
		err := encodeChunk(chunk, recursive, warn, consume)
		budget.Release(held)
		chunk, reserved, held = chunk[:0], 0, 0
		return err
	}
	for read := range reads {
		select {
		case <-read.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		result := read.result
		if result.err != nil {
			if !errors.Is(result.err, errArchived) {
				return result.err
			}
			// Archived in a way the listing does not show, such as an
			// archive tier of Intelligent-Tiering.
			result = readResult{file: result.file, skipped: errArchived.Error()}
		}
		// The same cut a local chunk makes: at most uploadBatchFiles names,
		// and content up to maxBatchContent unless the batch is empty.
		size := result.file.size
		if result.content == nil {
			size = 0
		}
		if len(chunk) == uploadBatchFiles || reserved > 0 && reserved+size > maxBatchContent {
			if err := flush(); err != nil {
				return err
			}
		}
		chunk = append(chunk, result)
		reserved += size
		held += read.held
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunk) == 0 {
		return nil
	}
	return flush()
}

// readS3Object reads one object of at most maxBatchContent bytes.
func readS3Object(ctx context.Context, file uploadFile) readResult {
	body, err := file.object.open(ctx)
	if err != nil {
		return readResult{file: file, err: err}
	}
	defer body.Close()
	content := make([]byte, file.size)
	if _, err := io.ReadFull(body, content); err != nil {
		return readResult{file: file, err: fmt.Errorf("read %s: %w", file.name(), err)}
	}
	// Reading to the end is what lets the SDK check the object's checksum.
	if extra, err := io.Copy(io.Discard, body); err != nil {
		return readResult{file: file, err: fmt.Errorf("read %s: %w", file.name(), err)}
	} else if extra > 0 {
		return readResult{file: file, err: fmt.Errorf("%s changed during the upload; upload it again", file.name())}
	}
	return readResult{file: file, content: content}
}

// uploadS3 uploads one object, or everything under a prefix, from S3.
//
// The source is counted while it uploads rather than before. A local tree is
// listed at over a million files a second, but an S3 listing is a request per
// thousand keys, so counting first would hold the first batch back for as
// long as the listing takes. The count runs beside the upload instead, and
// the progress line gains its total when the count ends.
func (a *app) uploadS3(ctx context.Context, organization, index string, location s3Location, prefix string, recursive bool) error {
	prefix, err := normalizeRemotePath(prefix, true)
	if err != nil {
		return fmt.Errorf("remote prefix: %w", err)
	}
	client, err := a.s3Client(ctx, location.bucket)
	if err != nil {
		return err
	}
	source, err := resolveS3Source(ctx, client, location, recursive)
	if err != nil {
		return err
	}

	status := a.newStatus()
	a.status.Store(status)
	defer a.status.Store(nil)
	status.start()
	defer status.finish()

	countCtx, stopCount := context.WithCancel(ctx)
	var counting sync.WaitGroup
	counting.Go(func() {
		// A failed count only leaves the line without a total; the upload's
		// own listing meets the same failure and reports it.
		total, err := source.walk(countCtx, "", nil, nil, func(files int64) { status.scanned(0, 0, files) })
		if err == nil {
			status.scanFinished(total)
		}
	})
	defer func() {
		stopCount()
		counting.Wait()
	}()

	var named int64
	err = a.uploadFiles(ctx, organization, index, status, source.object == nil, func(ctx context.Context, emit func(uploadFile) error) error {
		_, err := source.walk(ctx, prefix, func(file uploadFile) error {
			named++
			return emit(file)
		}, func(message string) { status.printf("gv: warning: %s\n", message) }, nil)
		return err
	}, source.encodeUploads)
	if err == nil && named == 0 {
		return errors.New("source contains no files")
	}
	return err
}

// isDirectoryBucket reports whether bucket names an S3 Express One Zone
// directory bucket, whose names have the form <base>--<zone-id>--x-s3.
func isDirectoryBucket(bucket string) bool {
	return strings.HasSuffix(bucket, "--x-s3")
}

// newS3Retryer returns the retry policy of the S3 client that reads a source.
// S3 answers overload with 503 SlowDown. The SDK's default policy gives up
// after three attempts and draws every retry from a shared quota, so one
// throttled listing could fail a long upload. This policy retries a throttled
// request with exponential backoff and jitter for longer and never runs out
// of quota. It stays in standard mode: adaptive mode paces every request of
// the client after one throttle response, which stalls every reader behind
// one slow prefix.
func newS3Retryer() aws.Retryer {
	return retry.NewStandard(func(standard *retry.StandardOptions) {
		standard.MaxAttempts = s3RetryMaxAttempts
		standard.MaxBackoff = s3RetryMaxBackoff
		standard.RateLimiter = ratelimit.None
	})
}
