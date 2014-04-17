// S3 interface
package s3

// FIXME need to prevent anything but ListDir working for s3://

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ncw/goamz/aws"
	"github.com/ncw/goamz/s3"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/swift"
)

// Register with Fs
func init() {
	fs.Register(&fs.FsInfo{
		Name:  "s3",
		NewFs: NewFs,
		// AWS endpoints: http://docs.amazonwebservices.com/general/latest/gr/rande.html#s3_region
		Options: []fs.Option{{
			Name: "access_key_id",
			Help: "AWS Access Key ID.",
		}, {
			Name: "secret_access_key",
			Help: "AWS Secret Access Key (password). ",
		}, {
			Name: "endpoint",
			Help: "Endpoint for S3 API.",
			Examples: []fs.OptionExample{{
				Value: "https://s3.amazonaws.com/",
				Help:  "The default endpoint - a good choice if you are unsure.\nUS Region, Northern Virginia or Pacific Northwest.\nLeave location constraint empty.",
			}, {
				Value: "https://s3-external-1.amazonaws.com",
				Help:  "US Region, Northern Virginia only.\nLeave location constraint empty.",
			}, {
				Value: "https://s3-us-west-2.amazonaws.com",
				Help:  "US West (Oregon) Region\nNeeds location constraint us-west-2.",
			}, {
				Value: "https://s3-us-west-1.amazonaws.com",
				Help:  "US West (Northern California) Region\nNeeds location constraint us-west-1.",
			}, {
				Value: "https://s3-eu-west-1.amazonaws.com",
				Help:  "EU (Ireland) Region Region\nNeeds location constraint EU or eu-west-1.",
			}, {
				Value: "https://s3-ap-southeast-1.amazonaws.com",
				Help:  "Asia Pacific (Singapore) Region\nNeeds location constraint ap-southeast-1.",
			}, {
				Value: "https://s3-ap-southeast-2.amazonaws.com",
				Help:  "Asia Pacific (Sydney) Region\nNeeds location constraint .",
			}, {
				Value: "https://s3-ap-northeast-1.amazonaws.com",
				Help:  "Asia Pacific (Tokyo) Region\nNeeds location constraint ap-northeast-1.",
			}, {
				Value: "https://s3-sa-east-1.amazonaws.com",
				Help:  "South America (Sao Paulo) Region\nNeeds location constraint sa-east-1.",
			}},
		}, {
			Name: "location_constraint",
			Help: "Location constraint - must be set to match the Endpoint.",
			Examples: []fs.OptionExample{{
				Value: "",
				Help:  "Empty for US Region, Northern Virginia or Pacific Northwest.",
			}, {
				Value: "us-west-2",
				Help:  "US West (Oregon) Region.",
			}, {
				Value: "us-west-1",
				Help:  "US West (Northern California) Region.",
			}, {
				Value: "eu-west-1",
				Help:  "EU (Ireland) Region.",
			}, {
				Value: "EU",
				Help:  "EU Region.",
			}, {
				Value: "ap-southeast-1",
				Help:  "Asia Pacific (Singapore) Region.",
			}, {
				Value: "ap-southeast-2",
				Help:  "Asia Pacific (Sydney) Region.",
			}, {
				Value: "ap-northeast-1",
				Help:  "Asia Pacific (Tokyo) Region.",
			}, {
				Value: "sa-east-1",
				Help:  "South America (Sao Paulo) Region.",
			}},
		}},
	})
}

// Constants
const (
	metaMtime = "X-Amz-Meta-Mtime" // the meta key to store mtime in
)

// FsS3 represents a remote s3 server
type FsS3 struct {
	c      *s3.S3     // the connection to the s3 server
	b      *s3.Bucket // the connection to the bucket
	bucket string     // the bucket we are working on
	perm   s3.ACL     // permissions for new buckets / objects
}

// FsObjectS3 describes a s3 object
type FsObjectS3 struct {
	// Will definitely have everything but meta which may be nil
	//
	// List will read everything but meta - to fill that in need to call
	// readMetaData
	s3           *FsS3      // what this object is part of
	remote       string     // The remote path
	etag         string     // md5sum of the object
	bytes        int64      // size of the object
	lastModified time.Time  // Last modified
	meta         s3.Headers // The object metadata if known - may be nil
}

// ------------------------------------------------------------

// String converts this FsS3 to a string
func (f *FsS3) String() string {
	return fmt.Sprintf("S3 bucket %s", f.bucket)
}

// Pattern to match a s3 path
var matcher = regexp.MustCompile(`^([^/]*)(.*)$`)

// parseParse parses a s3 'url'
func s3ParsePath(path string) (bucket, directory string, err error) {
	parts := matcher.FindStringSubmatch(path)
	if parts == nil {
		err = fmt.Errorf("Couldn't parse bucket out of s3 path %q", path)
	} else {
		bucket, directory = parts[1], parts[2]
		directory = strings.Trim(directory, "/")
	}
	return
}

// s3Connection makes a connection to s3
func s3Connection(name string) (*s3.S3, error) {
	// Make the auth
	accessKeyId := fs.ConfigFile.MustValue(name, "access_key_id")
	if accessKeyId == "" {
		return nil, errors.New("access_key_id not found")
	}
	secretAccessKey := fs.ConfigFile.MustValue(name, "secret_access_key")
	if secretAccessKey == "" {
		return nil, errors.New("secret_access_key not found")
	}
	auth := aws.Auth{AccessKey: accessKeyId, SecretKey: secretAccessKey}

	// FIXME look through all the regions by name and use one of them if found

	// Synthesize the region
	s3Endpoint := fs.ConfigFile.MustValue(name, "endpoint")
	if s3Endpoint == "" {
		s3Endpoint = "https://s3.amazonaws.com/"
	}
	region := aws.Region{
		Name:                 "s3",
		S3Endpoint:           s3Endpoint,
		S3LocationConstraint: false,
	}
	s3LocationConstraint := fs.ConfigFile.MustValue(name, "location_constraint")
	if s3LocationConstraint != "" {
		region.Name = s3LocationConstraint
		region.S3LocationConstraint = true
	}

	c := s3.New(auth, region)
	return c, nil
}

// NewFsS3 contstructs an FsS3 from the path, bucket:path
func NewFs(name, path string) (fs.Fs, error) {
	bucket, directory, err := s3ParsePath(path)
	if err != nil {
		return nil, err
	}
	if directory != "" {
		return nil, fmt.Errorf("Directories not supported yet in %q: %q", path, directory)
	}
	c, err := s3Connection(name)
	if err != nil {
		return nil, err
	}
	f := &FsS3{
		c:      c,
		bucket: bucket,
		b:      c.Bucket(bucket),
		perm:   s3.Private, // FIXME need user to specify
	}
	return f, nil
}

// Return an FsObject from a path
//
// May return nil if an error occurred
func (f *FsS3) NewFsObjectWithInfo(remote string, info *s3.Key) fs.Object {
	o := &FsObjectS3{
		s3:     f,
		remote: remote,
	}
	if info != nil {
		// Set info but not meta
		var err error
		o.lastModified, err = time.Parse(time.RFC3339, info.LastModified)
		if err != nil {
			fs.Log(o, "Failed to read last modified: %s", err)
			o.lastModified = time.Now()
		}
		o.etag = info.ETag
		o.bytes = info.Size
	} else {
		err := o.readMetaData() // reads info and meta, returning an error
		if err != nil {
			// logged already FsDebug("Failed to read info: %s", err)
			return nil
		}
	}
	return o
}

// Return an FsObject from a path
//
// May return nil if an error occurred
func (f *FsS3) NewFsObject(remote string) fs.Object {
	return f.NewFsObjectWithInfo(remote, nil)
}

// Walk the path returning a channel of FsObjects
func (f *FsS3) List() fs.ObjectsChan {
	out := make(fs.ObjectsChan, fs.Config.Checkers)
	go func() {
		// FIXME need to implement ALL loop
		objects, err := f.b.List("", "", "", 10000)
		if err != nil {
			fs.Stats.Error()
			log.Printf("Couldn't read bucket %q: %s", f.bucket, err)
		} else {
			for i := range objects.Contents {
				object := &objects.Contents[i]
				if fs := f.NewFsObjectWithInfo(object.Key, object); fs != nil {
					out <- fs
				}
			}
		}
		close(out)
	}()
	return out
}

// Lists the buckets
func (f *FsS3) ListDir() fs.DirChan {
	out := make(fs.DirChan, fs.Config.Checkers)
	go func() {
		defer close(out)
		buckets, err := f.c.ListBuckets()
		if err != nil {
			fs.Stats.Error()
			log.Printf("Couldn't list buckets: %s", err)
		} else {
			for _, bucket := range buckets {
				out <- &fs.Dir{
					Name:  bucket.Name,
					When:  bucket.CreationDate,
					Bytes: -1,
					Count: -1,
				}
			}
		}
	}()
	return out
}

// Put the FsObject into the bucket
func (f *FsS3) Put(in io.Reader, remote string, modTime time.Time, size int64) (fs.Object, error) {
	// Temporary FsObject under construction
	fs := &FsObjectS3{s3: f, remote: remote}

	// Set the mtime in the headers
	headers := s3.Headers{
		metaMtime: swift.TimeToFloatString(modTime),
	}

	// Guess the content type
	contentType := mime.TypeByExtension(path.Ext(remote))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	_, err := fs.s3.b.PutReaderHeaders(remote, in, size, contentType, f.perm, headers)
	return fs, err
}

// Mkdir creates the bucket if it doesn't exist
func (f *FsS3) Mkdir() error {
	err := f.b.PutBucket(f.perm)
	if err, ok := err.(*s3.Error); ok {
		if err.Code == "BucketAlreadyOwnedByYou" {
			return nil
		}
	}
	return err
}

// Rmdir deletes the bucket
//
// Returns an error if it isn't empty
func (f *FsS3) Rmdir() error {
	return f.b.DelBucket()
}

// Return the precision
func (f *FsS3) Precision() time.Duration {
	return time.Nanosecond
}

// ------------------------------------------------------------

// Return the parent Fs
func (o *FsObjectS3) Fs() fs.Fs {
	return o.s3
}

// Return a string version
func (o *FsObjectS3) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Return the remote path
func (o *FsObjectS3) Remote() string {
	return o.remote
}

// Md5sum returns the Md5sum of an object returning a lowercase hex string
func (o *FsObjectS3) Md5sum() (string, error) {
	return strings.Trim(strings.ToLower(o.etag), `"`), nil
}

// Size returns the size of an object in bytes
func (o *FsObjectS3) Size() int64 {
	return o.bytes
}

// readMetaData gets the metadata if it hasn't already been fetched
//
// it also sets the info
func (o *FsObjectS3) readMetaData() (err error) {
	if o.meta != nil {
		return nil
	}

	headers, err := o.s3.b.Head(o.remote, nil)
	if err != nil {
		fs.Debug(o, "Failed to read info: %s", err)
		return err
	}
	size, err := strconv.ParseInt(headers["Content-Length"], 10, 64)
	if err != nil {
		fs.Debug(o, "Failed to read size from: %q", headers)
		return err
	}
	o.etag = headers["Etag"]
	o.bytes = size
	o.meta = headers
	if o.lastModified, err = time.Parse(http.TimeFormat, headers["Last-Modified"]); err != nil {
		fs.Log(o, "Failed to read last modified from HEAD: %s", err)
		o.lastModified = time.Now()
	}
	return nil
}

// ModTime returns the modification time of the object
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *FsObjectS3) ModTime() time.Time {
	err := o.readMetaData()
	if err != nil {
		fs.Log(o, "Failed to read metadata: %s", err)
		return time.Now()
	}
	// read mtime out of metadata if available
	d, ok := o.meta[metaMtime]
	if !ok {
		// fs.Debug(o, "No metadata")
		return o.lastModified
	}
	modTime, err := swift.FloatStringToTime(d)
	if err != nil {
		fs.Log(o, "Failed to read mtime from object: %s", err)
		return o.lastModified
	}
	return modTime
}

// Sets the modification time of the local fs object
func (o *FsObjectS3) SetModTime(modTime time.Time) {
	err := o.readMetaData()
	if err != nil {
		fs.Stats.Error()
		fs.Log(o, "Failed to read metadata: %s", err)
		return
	}
	o.meta[metaMtime] = swift.TimeToFloatString(modTime)
	_, err = o.s3.b.Update(o.remote, o.s3.perm, o.meta)
	if err != nil {
		fs.Stats.Error()
		fs.Log(o, "Failed to update remote mtime: %s", err)
	}
}

// Is this object storable
func (o *FsObjectS3) Storable() bool {
	return true
}

// Open an object for read
func (o *FsObjectS3) Open() (in io.ReadCloser, err error) {
	in, err = o.s3.b.GetReader(o.remote)
	return
}

// Remove an object
func (o *FsObjectS3) Remove() error {
	return o.s3.b.Del(o.remote)
}

// Check the interfaces are satisfied
var _ fs.Fs = &FsS3{}
var _ fs.Object = &FsObjectS3{}