// Package storage wraps the S3-compatible bucket (Garage in our stack) that
// holds message attachments.
//
// Every object is stored with SSE-C: the API hands Garage a key (derived from
// ENCRYPTION_KEY) with each request, Garage encrypts what it stores and never
// keeps the key, so the bucket's volume is unreadable on its own. Range reads
// still work, so videos stream and seek as before.
package storage

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/url"
	"os"
	"time"

	"github.com/devstr0ke/chat-mixer/encryption"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"
)

var (
	client *minio.Client
	bucket string
	// sse encrypts new objects; retired holds older keys that objects may
	// still be under until EncryptExisting has re-encrypted them
	sse     encrypt.ServerSide
	retired []encrypt.ServerSide
)

func mustSSEC(key []byte) encrypt.ServerSide {
	s, err := encrypt.NewSSEC(key)
	if err != nil {
		log.Fatalf("storage: invalid file key: %v", err)
	}
	return s
}

// Connect configures the bucket client from the S3_* env vars. Attachments
// stay disabled when S3_ENDPOINT is unset.
func Connect() {
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		log.Println("storage: S3_ENDPOINT not set, attachments disabled")
		return
	}

	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		log.Fatalf("storage: invalid S3_ENDPOINT %q (expected e.g. http://garage:3900)", endpoint)
	}
	bucket = os.Getenv("S3_BUCKET")
	if bucket == "" {
		log.Fatal("storage: S3_BUCKET is not set")
	}
	region := os.Getenv("S3_REGION")
	if region == "" {
		region = "garage"
	}

	c, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), ""),
		Secure:       u.Scheme == "https",
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		log.Fatalf("storage: failed to create client: %v", err)
	}
	client = c
	currentKey, retiredKeys := encryption.FileKeys()
	sse = mustSSEC(currentKey)
	for _, k := range retiredKeys {
		retired = append(retired, mustSSEC(k))
	}

	// the bucket may come up after the API, so only warn
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ok, err := c.BucketExists(ctx, bucket); err != nil || !ok {
		log.Printf("storage: bucket %q not reachable yet (exists=%v, err=%v)", bucket, ok, err)
		return
	}
	log.Printf("storage: using bucket %q at %s", bucket, u.Host)
}

func Enabled() bool {
	return client != nil
}

func Put(ctx context.Context, key, contentType string, data []byte) error {
	_, err := client.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType, ServerSideEncryption: sse})
	return err
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// PutStream uploads size bytes from r without holding them in memory: large
// objects go up as a multipart upload, and when r is an io.ReaderAt (such as
// a multipart.File) each part is read straight from it, with no part buffers.
func PutStream(ctx context.Context, key, contentType string, r io.Reader, size int64) error {
	_, err := client.PutObject(ctx, bucket, key, r, size,
		minio.PutObjectOptions{ContentType: contentType, ServerSideEncryption: sse})
	return err
}

// Get opens an object for reading. It's seekable, so it can answer HTTP Range
// requests (needed to play and scrub videos). The caller must close it.
// Objects not yet re-encrypted (stored before encryption, or under a retired
// key) are still found, by trying those keys after the current one.
func Get(ctx context.Context, key string) (io.ReadSeekCloser, ObjectInfo, error) {
	var lastErr error
	for _, s := range append(append([]encrypt.ServerSide{sse}, retired...), nil) {
		obj, err := client.GetObject(ctx, bucket, key, minio.GetObjectOptions{ServerSideEncryption: s})
		if err != nil {
			return nil, ObjectInfo{}, err
		}
		// GetObject is lazy; Stat surfaces a missing object (or a wrong key)
		// before headers are written
		stat, err := obj.Stat()
		if err == nil {
			return obj, ObjectInfo{Size: stat.Size, ContentType: stat.ContentType}, nil
		}
		obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ObjectInfo{}, err
		}
		lastErr = err
	}
	return nil, ObjectInfo{}, lastErr
}

// EncryptExisting re-encrypts, in place, every object stored without
// encryption or under a retired key, and returns how many it changed. Garage
// copies them itself, so no file data passes through the API.
func EncryptExisting(ctx context.Context) (int, error) {
	done := 0
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return done, obj.Err
		}
		from, info, ok := needsEncryption(ctx, obj.Key)
		if !ok {
			continue
		}
		_, err := client.CopyObject(ctx,
			minio.CopyDestOptions{Bucket: bucket, Object: obj.Key, Encryption: sse,
				ReplaceMetadata: true, ContentType: info.ContentType},
			minio.CopySrcOptions{Bucket: bucket, Object: obj.Key, Encryption: from},
		)
		if err != nil {
			log.Printf("storage: failed to encrypt %s: %v", obj.Key, err)
			continue
		}
		done++
	}
	return done, nil
}

// needsEncryption reports whether an object isn't under the current key and,
// if so, which key (nil: none) it's readable with.
func needsEncryption(ctx context.Context, key string) (from encrypt.ServerSide, info minio.ObjectInfo, ok bool) {
	// checked without a key first: that only works for unencrypted objects
	if info, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{}); err == nil {
		return nil, info, true
	}
	if _, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{ServerSideEncryption: sse}); err == nil {
		return nil, minio.ObjectInfo{}, false
	}
	for _, s := range retired {
		if info, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{ServerSideEncryption: s}); err == nil {
			return s, info, true
		}
	}
	log.Printf("storage: %s isn't readable with any configured key", key)
	return nil, minio.ObjectInfo{}, false
}

func Delete(ctx context.Context, key string) error {
	return client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

// DeletePrefix removes every object under prefix, e.g. a deleted room's files.
func DeletePrefix(ctx context.Context, prefix string) error {
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if err := Delete(ctx, obj.Key); err != nil {
			return err
		}
	}
	return nil
}
