package cmd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/minio/cli"
	miniogo "github.com/minio/minio-go/v6"
	"github.com/minio/minio-go/v6/pkg/credentials"
	"github.com/minio/minio/pkg/dsync"
	"github.com/minio/sha256-simd"
	"gopkg.in/yaml.v2"

	"github.com/minio/minio-go/v6/pkg/encrypt"
	"github.com/minio/minio-go/v6/pkg/s3utils"
	"github.com/minio/minio/pkg/sync/errgroup"
	"github.com/minio/radio/cmd/logger"
	"github.com/minio/radio/pkg/streamdup"
)

func init() {
	miniogo.MaxRetry = 1
}

const radioTemplate = `NAME:
  {{.HelpName}} - {{.Usage}}

USAGE:
  {{.HelpName}} {{if .VisibleFlags}}[FLAGS]{{end}} [ENDPOINT...]
{{if .VisibleFlags}}
FLAGS:
  {{range .VisibleFlags}}{{.}}
  {{end}}{{end}}

EXAMPLES:
  1. Start radio server
     {{.Prompt}} {{.HelpName}} -c config.yml

  2. Start radio server with config from stdin
     {{.Prompt}} cat config.yml | {{.HelpName}} -c -
`

// Handler for 'minio radio s3' command line.
func radioMain(ctx *cli.Context) {
	reader, err := os.Open(ctx.String("config"))
	if err != nil {
		logger.FatalIf(err, "Invalid command line arguments")
	}

	data, err := ioutil.ReadAll(reader)
	if err != nil {
		logger.FatalIf(err, "Invalid command line arguments")
	}

	rconfig := radioConfig{}
	err = yaml.Unmarshal(data, &rconfig)
	if err != nil {
		logger.FatalIf(err, "Invalid command line arguments")
	}

	handleCommonCmdArgs(ctx)

	// Get port to listen on from radio address
	globalRadioHost, globalRadioPort = mustSplitHostPort(globalCLIContext.Addr)

	// On macOS, if a process already listens on LOCALIPADDR:PORT, net.Listen() falls back
	// to IPv6 address ie minio will start listening on IPv6 address whereas another
	// (non-)minio process is listening on IPv4 of given port.
	// To avoid this error situation we check for port availability.
	logger.FatalIf(checkPortAvailability(globalRadioHost, globalRadioPort), "Unable to start the radio")

	endpoints, err := createServerEndpoints(net.JoinHostPort(globalRadioHost, globalRadioPort), rconfig.Distribute.Peers)
	logger.FatalIf(err, "Invalid command line arguments")

	if len(endpoints) > 0 {
		startRadio(ctx, &Radio{endpoints: endpoints, rconfig: rconfig})
	} else {
		startRadio(ctx, &Radio{rconfig: rconfig})
	}
}

// Radio implements active/active radioted radio
type Radio struct {
	endpoints Endpoints
	rconfig   radioConfig
}

// newS3 - Initializes a new client by auto probing S3 server signature.
func newS3(bucket, urlStr, accessKey, secretKey, sessionToken string) (*miniogo.Core, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}
	options := miniogo.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, sessionToken),
		Secure:       u.Scheme == "https",
		Region:       s3utils.GetRegionFromURL(*u),
		BucketLookup: miniogo.BucketLookupAuto,
	}

	clnt, err := miniogo.NewWithOptions(u.Host, &options)
	if err != nil {
		return nil, err
	}

	// Set custom transport
	clnt.SetCustomTransport(NewCustomHTTPTransport())

	var retry int
	var maxRetry = 3
	for {
		// Check if the provided keys are valid.
		_, err = clnt.BucketExists(bucket)
		if err != nil {
			errResp := miniogo.ToErrorResponse(err)
			if errResp.Code == "XMinioServerNotInitialized" {
				logger.LogIf(context.Background(), err)
				time.Sleep(1 * time.Second)
				retry++
				continue
			}
			if retry < maxRetry {
				return nil, err
			}
		}
		break
	}

	return &miniogo.Core{Client: clnt}, nil
}

// ProtectionType different protection types
type ProtectionType string

// Different type of protection types.
const (
	MirrorType ProtectionType = "mirror"
)

type remoteConfig struct {
	Bucket       string `yaml:"bucket"`
	Endpoint     string `yaml:"endpoint"`
	AccessKey    string `yaml:"access_key"`
	SecretKey    string `yaml:"secret_key"`
	SessionToken string `yaml:"session_token"`
}

type bucketConfig struct {
	Bucket     string `yaml:"bucket"`
	AccessKey  string `yaml:"access_key"`
	SecretKey  string `yaml:"secret_key"`
	Protection struct {
		Scheme ProtectionType `json:"scheme"`
		Parity int            `json:"parity"`
	} `json:"protection"`
	Remotes []remoteConfig `yaml:"remote"`
}

// radioConfig radio configuration
type radioConfig struct {
	Certs struct {
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
		CAPath   string `yaml:"ca_path"`
	} `yaml:"certs"`
	Distribute struct {
		Peers string `yaml:"peers"`
		Token string `yaml:"token"`
	} `yaml:"distribute"`
	Buckets map[string]bucketConfig `json:"buckets"`
	Journal struct {
		Dir string `yaml:"dir"`
	} `yaml:"journal"`
}

type bucketClient struct {
	*miniogo.Core
	Bucket  string
	ID      string
	Offline int32
}

func (b *bucketClient) isOffline() bool {
	return atomic.LoadInt32(&b.Offline) == 0
}

type mirrorConfig struct {
	clnts []bucketClient
}

func clientID(cfg remoteConfig) string {
	hash := sha256.New()
	bytes, err := json.Marshal(&cfg)
	if err != nil {
		return fmt.Sprintf("%s%s", cfg.Bucket, cfg.Endpoint)
	}
	hash.Write(bytes)
	hashBytes := hash.Sum(nil)
	return hex.EncodeToString(hashBytes)
}

const healthCheckInterval = time.Second * 5

func newBucketClients(bcfgs []remoteConfig) ([]bucketClient, error) {
	var clnts []bucketClient
	for _, bCfg := range bcfgs {
		clnt, err := newS3(bCfg.Bucket, bCfg.Endpoint, bCfg.AccessKey, bCfg.SecretKey, bCfg.SessionToken)
		if err != nil {
			return nil, err
		}

		clnts = append(clnts, bucketClient{
			Core:   clnt,
			Bucket: bCfg.Bucket,
			ID:     clientID(bCfg),
		})
	}
	go func() {
		for {
			g := errgroup.WithNErrs(len(clnts))
			for index := range clnts {
				index := index
				g.Go(func() error {
					var perr error
					_, perr = clnts[index].BucketExists(clnts[index].Bucket)
					return perr
				}, index)
			}
			for index, err := range g.Wait() {
				if err != nil {
					atomic.StoreInt32(&clnts[index].Offline, 0)
				} else {
					atomic.StoreInt32(&clnts[index].Offline, 1)
				}
			}
			select {
			case <-GlobalContext.Done():
				return
			default:
				time.Sleep(healthCheckInterval)
			}
		}
	}()
	return clnts, nil
}

// NewRadioLayer returns s3 ObjectLayer.
func (g *Radio) NewRadioLayer() (ObjectLayer, error) {
	var radioLockers []dsync.NetLocker
	for _, endpoint := range g.endpoints {
		radioLockers = append(radioLockers, newLockAPI(endpoint, g.rconfig.Distribute.Token))
	}

	s := radioObjects{
		multipartUploadIDMap: make(map[string][]string),
		endpoints:            g.endpoints,
		radioLockers:         radioLockers,
		nsMutex:              newNSLock(len(radioLockers) > 0),
		mirrorClients:        make(map[string]mirrorConfig),
		journalDir:           g.rconfig.Journal.Dir,
	}

	// creds are ignored here, since S3 radio implements chaining all credentials.
	for bucket, cfg := range g.rconfig.Buckets {
		if len(cfg.Remotes) != 2 {
			return nil, fmt.Errorf("Invalid remote configuration specified for %s,expecting 2 remotes", bucket)
		}
		clnts, err := newBucketClients(cfg.Remotes)
		if err != nil {
			return nil, err
		}
		if cfg.Protection.Scheme == MirrorType {
			s.mirrorClients[bucket] = mirrorConfig{
				clnts: clnts,
			}
		}
	}

	return &s, nil
}

// Production - radio radio is not yet production ready.
func (g *Radio) Production() bool {
	return true
}

// radioObjects implements radio for MinIO and S3 compatible object storage servers.
type radioObjects struct {
	endpoints            Endpoints
	radioLockers         []dsync.NetLocker
	mirrorClients        map[string]mirrorConfig
	multipartUploadIDMap map[string][]string
	nsMutex              *NSLockMap
	journalDir           string
}

func (l *radioObjects) NewNSLock(ctx context.Context, bucket string, object string) RWLocker {
	return l.nsMutex.NewNSLock(ctx, func() []dsync.NetLocker {
		return l.radioLockers
	}, bucket, object)
}

// GetBucketInfo gets bucket metadata..
func (l *radioObjects) GetBucketInfo(ctx context.Context, bucket string) (bi BucketInfo, e error) {
	_, ok := l.mirrorClients[bucket]
	if !ok {
		return bi, BucketNotFound{Bucket: bucket}
	}
	return BucketInfo{
		Name:    bucket,
		Created: time.Now().UTC(),
	}, nil
}

// ListBuckets lists all S3 buckets
func (l *radioObjects) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	var b []BucketInfo
	for bucket := range l.mirrorClients {
		b = append(b, BucketInfo{
			Name:    bucket,
			Created: time.Now().UTC(),
		})
	}
	return b, nil
}

// ListObjects lists all blobs in S3 bucket filtered by prefix
func (l *radioObjects) ListObjects(ctx context.Context, bucket string, prefix string, marker string, delimiter string, maxKeys int) (loi ListObjectsInfo, e error) {
	rs3, ok := l.mirrorClients[bucket]
	if !ok {
		return loi, BucketNotFound{
			Bucket: bucket,
		}
	}

	var err error
	for _, clnt := range rs3.clnts {
		var result miniogo.ListBucketResult
		result, err = clnt.ListObjects(clnt.Bucket, prefix, marker, delimiter, maxKeys)
		if err != nil {
			continue
		}
		return FromMinioClientListBucketResult(bucket, result), nil
	}
	return loi, ErrorRespToObjectError(err, bucket)
}

// ListObjectsV2 lists all blobs in S3 bucket filtered by prefix
func (l *radioObjects) ListObjectsV2(ctx context.Context, bucket, prefix, continuationToken, delimiter string, maxKeys int, fetchOwner bool, startAfter string) (loi ListObjectsV2Info, e error) {
	rs3, ok := l.mirrorClients[bucket]
	if !ok {
		return loi, BucketNotFound{
			Bucket: bucket,
		}
	}
	var err error
	for _, clnt := range rs3.clnts {
		var result miniogo.ListBucketV2Result
		result, err = clnt.ListObjectsV2(clnt.Bucket, prefix,
			continuationToken, fetchOwner, delimiter,
			maxKeys, startAfter)
		if err != nil {
			continue
		}
		return FromMinioClientListBucketV2Result(bucket, result), nil
	}
	return loi, ErrorRespToObjectError(err, bucket)
}

// GetObjectNInfo - returns object info and locked object ReadCloser
func (l *radioObjects) GetObjectNInfo(ctx context.Context, bucket, object string, rs *HTTPRangeSpec, h http.Header, lockType LockType, o ObjectOptions) (gr *GetObjectReader, err error) {
	var nsUnlocker = func() {}

	// Acquire lock
	if lockType != NoLock {
		lock := l.NewNSLock(ctx, bucket, object)
		switch lockType {
		case WriteLock:
			if err = lock.GetLock(globalObjectTimeout); err != nil {
				return nil, err
			}
			nsUnlocker = lock.Unlock
		case ReadLock:
			if err = lock.GetRLock(globalObjectTimeout); err != nil {
				return nil, err
			}
			nsUnlocker = lock.RUnlock
		}
	}

	rs3s, ok := l.mirrorClients[bucket]
	if !ok {
		return nil, BucketNotFound{
			Bucket: bucket,
		}
	}

	info, err := l.getObjectInfo(ctx, bucket, object, o)
	if err != nil {
		return nil, ErrorRespToObjectError(err, bucket, object)
	}

	startOffset, length, err := rs.GetOffsetLength(info.Size)
	if err != nil {
		return nil, ErrorRespToObjectError(err, bucket, object)
	}

	pr, pw := io.Pipe()
	go func() {
		opts := miniogo.GetObjectOptions{}
		opts.ServerSideEncryption = o.ServerSideEncryption

		if startOffset >= 0 && length >= 0 {
			if err := opts.SetRange(startOffset, startOffset+length-1); err != nil {
				pw.CloseWithError(ErrorRespToObjectError(err, bucket, object))
				return
			}
		}

		reader, _, _, err := rs3s.clnts[info.ReplicaIndex].GetObjectWithContext(
			ctx,
			rs3s.clnts[info.ReplicaIndex].Bucket,
			object, opts)
		if err != nil {
			pw.CloseWithError(ErrorRespToObjectError(err, bucket, object))
			return
		}
		defer reader.Close()

		_, err = io.Copy(pw, reader)
		pw.CloseWithError(ErrorRespToObjectError(err, bucket, object))
	}()

	// Setup cleanup function to cause the above go-routine to
	// exit in case of partial read
	pipeCloser := func() { pr.Close() }
	return NewGetObjectReaderFromReader(pr, info, o, pipeCloser, nsUnlocker)
}

// GetObject reads an object from S3. Supports additional
// parameters like offset and length which are synonymous with
// HTTP Range requests.
//
// startOffset indicates the starting read location of the object.
// length indicates the total length of the object.
func (l *radioObjects) GetObject(ctx context.Context, bucket string, object string, startOffset int64, length int64, writer io.Writer, etag string, o ObjectOptions) error {
	return NotImplemented{}
}

func (l *radioObjects) getObjectInfo(ctx context.Context, bucket string, object string, opts ObjectOptions) (objInfo ObjectInfo, err error) {
	rs3s, ok := l.mirrorClients[bucket]
	if !ok {
		return ObjectInfo{}, BucketNotFound{
			Bucket: bucket,
		}
	}
	rIndex := []int{0, 1} // find remotes that are online
	for index, clnt := range rs3s.clnts {
		if clnt.isOffline() {
			rIndex[index] = -1
			continue
		}
		jDir := globalHealSys.getJournalDir(clnt.Bucket, bucket, object)
		jlog, jerr := globalHealSys.readJournalEntry(ctx, jDir)
		if jerr == nil && jlog.ErrClientID == clnt.ID {
			rIndex[index] = -1
		}
	}

	oinfos := make([]miniogo.ObjectInfo, len(rs3s.clnts))
	g := errgroup.WithNErrs(len(rs3s.clnts))
	for index := range rs3s.clnts {
		if rIndex[index] == -1 { // skip offline remotes
			continue
		}
		index := index
		g.Go(func() error {
			nctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			var perr error
			oinfos[index], perr = rs3s.clnts[index].StatObjectWithContext(
				nctx,
				rs3s.clnts[index].Bucket, object,
				miniogo.StatObjectOptions{
					GetObjectOptions: miniogo.GetObjectOptions{
						ServerSideEncryption: opts.ServerSideEncryption,
					},
				})
			return perr
		}, index)
	}
	for idx, err := range g.Wait() {
		if rIndex[idx] == -1 {
			continue
		}
		if err == nil {
			return FromMinioClientObjectInfo(bucket, oinfos[idx], idx), nil
		}
		return ObjectInfo{}, ErrorRespToObjectError(err, bucket, object)
	}
	return ObjectInfo{}, BackendDown{}
}

// GetObjectInfo reads object info and replies back ObjectInfo
func (l *radioObjects) GetObjectInfo(ctx context.Context, bucket string, object string, opts ObjectOptions) (objInfo ObjectInfo, err error) {
	// Lock the object before reading.
	objectLock := l.NewNSLock(ctx, bucket, object)
	if err := objectLock.GetRLock(globalObjectTimeout); err != nil {
		return ObjectInfo{}, err
	}
	defer objectLock.RUnlock()
	return l.getObjectInfo(ctx, bucket, object, opts)
}

// PutObject creates a new object with the incoming data,
func (l *radioObjects) PutObject(ctx context.Context, bucket string, object string, r *PutObjReader, opts ObjectOptions) (objInfo ObjectInfo, err error) {
	data := r.Reader
	// Lock the object before reading.
	objectLock := l.NewNSLock(ctx, bucket, object)
	if err := objectLock.GetLock(globalObjectTimeout); err != nil {
		return ObjectInfo{}, err
	}
	defer objectLock.Unlock()

	rs3s, ok := l.mirrorClients[bucket]
	if !ok {
		return objInfo, BucketNotFound{Bucket: bucket}
	}

	readers, err := streamdup.New(data, len(rs3s.clnts))
	if err != nil {
		return objInfo, ErrorRespToObjectError(err, bucket, object)
	}
	radioTagID := mustGetUUID()
	opts.UserDefined["x-amz-meta-radio-tag"] = radioTagID

	oinfos := make([]miniogo.ObjectInfo, len(rs3s.clnts))
	g := errgroup.WithNErrs(len(rs3s.clnts))
	for index := range rs3s.clnts {
		index := index
		g.Go(func() error {
			var perr error
			oinfos[index], perr = rs3s.clnts[index].PutObjectWithContext(ctx,
				rs3s.clnts[index].Bucket, object,
				readers[index], data.Size(),
				data.MD5Base64String(), data.SHA256HexString(),
				ToMinioClientMetadata(opts.UserDefined), opts.ServerSideEncryption)
			oinfos[index].Key = object
			oinfos[index].Metadata = ToMinioClientObjectInfoMetadata(opts.UserDefined)
			return perr
		}, index)
	}

	errs := g.Wait()

	rindex, err := reduceWriteErrs(errs)
	if err != nil {
		return objInfo, ErrorRespToObjectError(err, bucket, object)
	}

	for index, perr := range errs {
		if perr != nil {
			globalHealSys.send(ctx, journalEntry{Bucket: bucket, Object: object, ErrClientID: rs3s.clnts[index].ID, SrcClientID: rs3s.clnts[rindex].ID, ReplicaBucket: rs3s.clnts[index].Bucket, Timestamp: time.Now(), Op: opPutObject, ETag: oinfos[rindex].ETag, RadioTagID: radioTagID, UserMeta: ToMinioClientMetadata(opts.UserDefined), ServerSideEncryption: opts.ServerSideEncryption})
		}
	}
	return FromMinioClientObjectInfo(bucket, oinfos[rindex], rindex), nil
}

// CopyObject copies an object from source bucket to a destination bucket.
func (l *radioObjects) CopyObject(ctx context.Context, srcBucket string, srcObject string, dstBucket string, dstObject string, srcInfo ObjectInfo, srcOpts, dstOpts ObjectOptions) (objInfo ObjectInfo, err error) {
	// Check if this request is only metadata update.
	cpSrcDstSame := isStringEqual(pathJoin(srcBucket, srcObject), pathJoin(dstBucket, dstObject))
	if !cpSrcDstSame {
		objectLock := l.NewNSLock(ctx, dstBucket, dstObject)
		if err = objectLock.GetLock(globalObjectTimeout); err != nil {
			return objInfo, err
		}
		defer objectLock.Unlock()
	}

	if srcOpts.CheckCopyPrecondFn != nil && srcOpts.CheckCopyPrecondFn(srcInfo, srcInfo.ETag) {
		return ObjectInfo{}, PreConditionFailed{}
	}
	// Set this header such that following CopyObject() always sets the right metadata on the destination.
	// metadata input is already a trickled down value from interpreting x-amz-metadata-directive at
	// handler layer. So what we have right now is supposed to be applied on the destination object anyways.
	// So preserve it by adding "REPLACE" directive to save all the metadata set by CopyObject API.
	srcInfo.UserDefined["x-amz-metadata-directive"] = "REPLACE"
	srcInfo.UserDefined["x-amz-copy-source-if-match"] = srcInfo.ETag
	header := make(http.Header)
	if srcOpts.ServerSideEncryption != nil {
		encrypt.SSECopy(srcOpts.ServerSideEncryption).Marshal(header)
	}

	if dstOpts.ServerSideEncryption != nil {
		dstOpts.ServerSideEncryption.Marshal(header)
	}
	for k, v := range header {
		srcInfo.UserDefined[k] = v[0]
	}

	rs3sSrc := l.mirrorClients[srcBucket]
	rs3sDest := l.mirrorClients[dstBucket]
	if len(rs3sSrc.clnts) != len(rs3sDest.clnts) {
		return objInfo, errors.New("unexpected")
	}

	n := len(rs3sDest.clnts)
	oinfos := make([]miniogo.ObjectInfo, n)

	g := errgroup.WithNErrs(n)
	for index := 0; index < n; index++ {
		index := index
		g.Go(func() error {
			var err error
			oinfos[index], err = rs3sSrc.clnts[index].CopyObjectWithContext(
				ctx,
				rs3sSrc.clnts[index].Bucket, srcObject,
				rs3sDest.clnts[index].Bucket, dstObject, srcInfo.UserDefined)
			return err
		}, index)
	}

	errs := g.Wait()
	var (
		oerr       error
		radioTagID string
	)
	rindex, err := reduceWriteErrs(errs)
	if err != nil {
		return objInfo, ErrorRespToObjectError(err, srcBucket, srcObject)
	}
	objInfo, oerr = l.getObjectInfo(ctx, dstBucket, dstObject, dstOpts)

	radioTagID = objInfo.UserDefined["X-Amz-Meta-Radio-Tag"]

	for index, err := range errs {
		if err != nil {
			globalHealSys.send(ctx, journalEntry{Bucket: srcBucket, Object: srcObject, DstBucket: dstBucket, DstObject: dstObject, ReplicaBucket: rs3sSrc.clnts[index].Bucket, ErrClientID: rs3sSrc.clnts[index].ID, SrcClientID: rs3sSrc.clnts[rindex].ID, Timestamp: time.Now(), Op: opCopyObject, RadioTagID: radioTagID})
		}
	}
	return objInfo, oerr
}

// DeleteObject deletes a blob in bucket
func (l *radioObjects) DeleteObject(ctx context.Context, bucket string, object string) error {
	objectLock := l.NewNSLock(ctx, bucket, object)
	if err := objectLock.GetLock(globalObjectTimeout); err != nil {
		return err
	}
	defer objectLock.Unlock()

	rs3s, ok := l.mirrorClients[bucket]
	if !ok {
		return BucketNotFound{
			Bucket: bucket,
		}
	}

	n := len(rs3s.clnts)
	g := errgroup.WithNErrs(n)
	for index := 0; index < n; index++ {
		index := index
		g.Go(func() error {
			return rs3s.clnts[index].RemoveObject(rs3s.clnts[index].Bucket, object)
		}, index)
	}
	errs := g.Wait()
	rindex, err := reduceWriteErrs(errs)
	if err != nil {
		return err
	}
	for index, err := range errs {
		if err != nil {
			globalHealSys.send(ctx, journalEntry{Bucket: bucket, Object: object, ReplicaBucket: rs3s.clnts[index].Bucket, ErrClientID: rs3s.clnts[index].ID, SrcClientID: rs3s.clnts[rindex].ID, Timestamp: time.Now(), Op: opDeleteObject})
		}
	}
	return nil
}

func (l *radioObjects) DeleteObjects(ctx context.Context, bucket string, objects []string) ([]error, error) {
	errs := make([]error, len(objects))

	objectLock := l.NewNSLock(ctx, bucket, "")
	if err := objectLock.GetLock(globalObjectTimeout); err != nil {
		return errs, err
	}
	defer objectLock.Unlock()

	rs3s, ok := l.mirrorClients[bucket]
	if !ok {
		return errs, BucketNotFound{
			Bucket: bucket,
		}
	}

	n := len(rs3s.clnts)
	objectsChs := make([]chan string, n)
	for index := 0; index < n; index++ {
		index := index
		objectsChs[index] = make(chan string)
		go func() {
			defer close(objectsChs[index])
			for _, object := range objects {
				objectsChs[index] <- object
			}
		}()
	}
	var errsCh = make([]<-chan miniogo.RemoveObjectError, n)
	var offlines = make([]bool, len(rs3s.clnts))
	for index := 0; index < n; index++ {
		errsCh[index] = rs3s.clnts[index].RemoveObjectsWithContext(ctx,
			rs3s.clnts[index].Bucket,
			objectsChs[index])
		offlines[index] = rs3s.clnts[index].isOffline()
	}

	multiObjectError := make(map[string][]error)
	var exitCount int
	for {
		for _, errCh := range errsCh {
			select {
			case err, ok := <-errCh:
				if !ok {
					exitCount++
					break
				}
				multiObjectError[err.ObjectName] = append(
					multiObjectError[err.ObjectName],
					err.Err,
				)
			default:
				continue
			}
		}
		if exitCount == len(errsCh) {
			break
		}
	}
	for objName, errs := range multiObjectError {
		for idx, robjName := range objects {
			if objName == robjName {
				rindex, err := reduceWriteErrs(errs)
				if err != nil {
					errs[idx] = err
				}
				for index, err := range errs {
					if err != nil {
						globalHealSys.send(ctx, journalEntry{Bucket: bucket, Object: objName, ReplicaBucket: rs3s.clnts[index].Bucket, ErrClientID: rs3s.clnts[index].ID, SrcClientID: rs3s.clnts[rindex].ID, Timestamp: time.Now(), Op: opDeleteObject})
					}
				}
			}
		}
	}
	for i, offline := range offlines {
		if offline {
			for _, obj := range objects {
				globalHealSys.send(ctx, journalEntry{Bucket: bucket, Object: obj, ReplicaBucket: rs3s.clnts[i].Bucket, ErrClientID: rs3s.clnts[i].ID, Timestamp: time.Now(), Op: opDeleteObject})

			}
		}
	}
	return errs, nil
}

// ListMultipartUploads lists all multipart uploads.
func (l *radioObjects) ListMultipartUploads(ctx context.Context, bucket string, prefix string, keyMarker string, uploadIDMarker string, delimiter string, maxUploads int) (lmi ListMultipartsInfo, e error) {
	rs3, ok := l.mirrorClients[bucket]
	if !ok {
		return lmi, BucketNotFound{Bucket: bucket}
	}

	var err error
	for _, clnt := range rs3.clnts {
		var result miniogo.ListMultipartUploadsResult
		result, err = clnt.ListMultipartUploads(clnt.Bucket, prefix,
			keyMarker, uploadIDMarker, delimiter, maxUploads)
		if err != nil {
			continue
		}
		return FromMinioClientListMultipartsInfo(result), nil
	}
	return lmi, ErrorRespToObjectError(err, bucket)
}

// NewMultipartUpload upload object in multiple parts
func (l *radioObjects) NewMultipartUpload(ctx context.Context, bucket string, object string, o ObjectOptions) (string, error) {

	o.UserDefined["x-amz-meta-radio-tag"] = mustGetUUID()

	// Create PutObject options
	opts := miniogo.PutObjectOptions{UserMetadata: o.UserDefined, ServerSideEncryption: o.ServerSideEncryption}
	uploadID := mustGetUUID()

	uploadIDLock := l.NewNSLock(ctx, bucket, pathJoin(object, uploadID))
	if err := uploadIDLock.GetLock(globalOperationTimeout); err != nil {
		return uploadID, err
	}
	defer uploadIDLock.Unlock()

	rs3s, ok := l.mirrorClients[bucket]
	if !ok {
		return uploadID, BucketNotFound{Bucket: bucket}
	}

	for _, clnt := range rs3s.clnts {
		id, err := clnt.NewMultipartUpload(clnt.Bucket, object, opts)
		if err != nil {
			// Abort any failed uploads to one of the radios
			clnt.AbortMultipartUpload(clnt.Bucket, object, uploadID)
			return uploadID, ErrorRespToObjectError(err, bucket, object)
		}
		l.multipartUploadIDMap[uploadID] = append(l.multipartUploadIDMap[uploadID], id)

	}
	return uploadID, nil
}

// PutObjectPart puts a part of object in bucket
func (l *radioObjects) PutObjectPart(ctx context.Context, bucket string, object string, uploadID string, partID int, r *PutObjReader, opts ObjectOptions) (pi PartInfo, e error) {
	data := r.Reader

	uploadIDLock := l.NewNSLock(ctx, bucket, pathJoin(object, uploadID))
	if err := uploadIDLock.GetLock(globalOperationTimeout); err != nil {
		return pi, err
	}
	defer uploadIDLock.Unlock()

	uploadIDs, ok := l.multipartUploadIDMap[uploadID]
	if !ok {
		return pi, InvalidUploadID{
			Bucket:   bucket,
			Object:   object,
			UploadID: uploadID,
		}
	}

	rs3s := l.mirrorClients[bucket]

	readers, err := streamdup.New(data, len(rs3s.clnts))
	if err != nil {
		return pi, err
	}

	pinfos := make([]miniogo.ObjectPart, len(rs3s.clnts))
	g := errgroup.WithNErrs(len(rs3s.clnts))
	for index := range rs3s.clnts {
		index := index
		g.Go(func() error {
			var err error
			pinfos[index], err = rs3s.clnts[index].PutObjectPartWithContext(
				ctx,
				rs3s.clnts[index].Bucket, object,
				uploadIDs[index], partID, readers[index], data.Size(),
				data.MD5Base64String(), data.SHA256HexString(), opts.ServerSideEncryption)
			return err
		}, index)
	}
	rindex, err := reduceWriteErrs(g.Wait())
	if err != nil {
		return pi, ErrorRespToObjectError(err, bucket, object)
	}

	return FromMinioClientObjectPart(pinfos[rindex]), nil
}

// CopyObjectPart creates a part in a multipart upload by copying
// existing object or a part of it.
func (l *radioObjects) CopyObjectPart(ctx context.Context, srcBucket, srcObject, destBucket, destObject, uploadID string,
	partID int, startOffset, length int64, srcInfo ObjectInfo, srcOpts, dstOpts ObjectOptions) (p PartInfo, err error) {

	uploadIDLock := l.NewNSLock(ctx, destBucket, pathJoin(destObject, uploadID))
	if err := uploadIDLock.GetLock(globalOperationTimeout); err != nil {
		return p, err
	}
	defer uploadIDLock.Unlock()

	if srcOpts.CheckCopyPrecondFn != nil && srcOpts.CheckCopyPrecondFn(srcInfo, srcInfo.ETag) {
		return PartInfo{}, PreConditionFailed{}
	}
	srcInfo.UserDefined = map[string]string{
		"x-amz-copy-source-if-match": srcInfo.ETag,
	}
	header := make(http.Header)
	if srcOpts.ServerSideEncryption != nil {
		encrypt.SSECopy(srcOpts.ServerSideEncryption).Marshal(header)
	}

	if dstOpts.ServerSideEncryption != nil {
		dstOpts.ServerSideEncryption.Marshal(header)
	}
	for k, v := range header {
		srcInfo.UserDefined[k] = v[0]
	}

	uploadIDs, ok := l.multipartUploadIDMap[uploadID]
	if !ok {
		return p, InvalidUploadID{
			Bucket:   srcBucket,
			Object:   srcObject,
			UploadID: uploadID,
		}
	}

	rs3sSrc := l.mirrorClients[srcBucket]
	rs3sDest := l.mirrorClients[destBucket]

	if len(rs3sSrc.clnts) != len(rs3sDest.clnts) {
		return p, errors.New("unexpected")
	}

	n := len(rs3sDest.clnts)
	pinfos := make([]miniogo.CompletePart, n)

	g := errgroup.WithNErrs(n)
	for index := 0; index < n; index++ {
		index := index
		g.Go(func() error {
			var err error
			pinfos[index], err = rs3sSrc.clnts[index].CopyObjectPartWithContext(
				ctx,
				rs3sSrc.clnts[index].Bucket,
				srcObject, rs3sDest.clnts[index].Bucket, destObject,
				uploadIDs[index], partID, startOffset, length, srcInfo.UserDefined)
			return err
		}, index)
	}
	rindex, err := reduceWriteErrs(g.Wait())
	if err != nil {
		return p, ErrorRespToObjectError(err, srcBucket, srcObject)
	}

	p.PartNumber = pinfos[rindex].PartNumber
	p.ETag = pinfos[rindex].ETag
	return p, nil
}

// ListObjectParts returns all object parts for specified object in specified bucket
func (l *radioObjects) ListObjectParts(ctx context.Context, bucket string, object string, uploadID string, partNumberMarker int, maxParts int, opts ObjectOptions) (lpi ListPartsInfo, e error) {
	return lpi, nil
}

// AbortMultipartUpload aborts a ongoing multipart upload
func (l *radioObjects) AbortMultipartUpload(ctx context.Context, bucket string, object string, uploadID string) error {
	uploadIDLock := l.NewNSLock(ctx, bucket, pathJoin(object, uploadID))
	if err := uploadIDLock.GetLock(globalOperationTimeout); err != nil {
		return err
	}
	defer uploadIDLock.Unlock()

	uploadIDs, ok := l.multipartUploadIDMap[uploadID]
	if !ok {
		return InvalidUploadID{
			Bucket:   bucket,
			Object:   object,
			UploadID: uploadID,
		}
	}

	rs3s := l.mirrorClients[bucket]
	for index, id := range uploadIDs {
		if err := rs3s.clnts[index].AbortMultipartUploadWithContext(
			ctx, rs3s.clnts[index].Bucket, object, id); err != nil {
			return ErrorRespToObjectError(err, bucket, object)
		}
	}
	delete(l.multipartUploadIDMap, uploadID)
	return nil
}

// CompleteMultipartUpload completes ongoing multipart upload and finalizes object
func (l *radioObjects) CompleteMultipartUpload(ctx context.Context, bucket string, object string, uploadID string, uploadedParts []CompletePart, opts ObjectOptions) (oi ObjectInfo, err error) {

	// Hold read-locks to verify uploaded parts, also disallows
	// parallel part uploads as well.
	uploadIDLock := l.NewNSLock(ctx, bucket, pathJoin(object, uploadID))
	if err = uploadIDLock.GetRLock(globalOperationTimeout); err != nil {
		return oi, err
	}
	defer uploadIDLock.RUnlock()

	// Hold namespace to complete the transaction, only hold
	// if uploadID can be held exclusively.
	objectLock := l.NewNSLock(ctx, bucket, object)
	if err = objectLock.GetLock(globalOperationTimeout); err != nil {
		return oi, err
	}
	defer objectLock.Unlock()

	uploadIDs, ok := l.multipartUploadIDMap[uploadID]
	if !ok {
		return oi, InvalidUploadID{
			Bucket:   bucket,
			Object:   object,
			UploadID: uploadID,
		}
	}

	rs3s := l.mirrorClients[bucket]
	var etag string
	errs := make([]error, len(uploadIDs))
	hasErr := false
	for index, id := range uploadIDs {
		etag, errs[index] = rs3s.clnts[index].CompleteMultipartUploadWithContext(
			ctx,
			rs3s.clnts[index].Bucket,
			object, id, ToMinioClientCompleteParts(uploadedParts))
		hasErr = hasErr || (errs[index] != nil)
	}
	rindex, err := reduceWriteErrs(errs)
	if err != nil {
		return oi, ErrorRespToObjectError(err, bucket, object)
	}

	delete(l.multipartUploadIDMap, uploadID)
	radioTagID := ""
	var userMeta map[string]string
	var sses3 encrypt.ServerSide
	if hasErr {
		objInfo, err := l.getObjectInfo(ctx, bucket, object, opts)
		if err == nil {
			radioTagID = objInfo.UserDefined["X-Amz-Meta-Radio-Tag"]
			userMeta = objectInfoToMetadata(oi)
			if _, ok := objInfo.UserDefined["X-Amz-Server-Side-Encryption"]; ok {
				sses3 = encrypt.NewSSE()
			}
		}
	}
	for index, perr := range errs {
		if perr != nil {
			globalHealSys.send(ctx, journalEntry{Bucket: bucket, Object: object, ReplicaBucket: rs3s.clnts[index].Bucket, ErrClientID: rs3s.clnts[index].ID, SrcClientID: rs3s.clnts[rindex].ID, Timestamp: time.Now(), Op: opPutObject, ETag: etag, RadioTagID: radioTagID, UserMeta: userMeta, ServerSideEncryption: sses3})
		}
	}
	return ObjectInfo{Bucket: bucket, Name: object, ETag: etag}, nil
}
