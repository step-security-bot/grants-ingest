package main

import (
	"bytes"
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3GetObjectAPI is the interface for retrieving objects from an S3 bucket
type S3GetObjectAPI interface {
	// GetObject retrieves an object from S3
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// S3ReadObjectAPI is the interface for reading object contents and metadata from an S3 bucket
type S3ReadObjectAPI interface {
	S3GetObjectAPI
	s3.HeadObjectAPIClient
}

// S3PutObjectAPI is the interface for writing new or replacement objects in an S3 bucket
type S3PutObjectAPI interface {
	// PutObject uploads an object to S3
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3ReadWriteObjectAPI is the interface for reading to and writing from an S3 bucket
type S3ReadWriteObjectAPI interface {
	S3ReadObjectAPI
	S3PutObjectAPI
}

// UploadS3Object uploads bytes read from from r to an S3 object at the given bucket and key.
// If an error was encountered during upload, returns the error.
// Returns nil when the upload was successful.
func UploadS3Object(ctx context.Context, c S3PutObjectAPI, bucket, key string, b []byte) error {
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               aws.String(bucket),
		Key:                  aws.String(key),
		Body:                 bytes.NewReader(b),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	})
	return err
}
