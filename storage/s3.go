// Package storage wraps the S3-compatible bucket (Garage in our stack) that
// holds message attachments.
package storage

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/url"
	"os"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	client *minio.Client
	bucket string
)

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
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// Get opens an object for streaming. The caller must close it.
func Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	obj, err := client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	// GetObject is lazy; Stat surfaces a missing object before headers are written
	stat, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, ObjectInfo{}, err
	}
	return obj, ObjectInfo{Size: stat.Size, ContentType: stat.ContentType}, nil
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
