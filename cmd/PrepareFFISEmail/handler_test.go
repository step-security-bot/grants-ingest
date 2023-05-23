package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	goenv "github.com/Netflix/go-env"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsTransport "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/go-kit/log"
	"github.com/hashicorp/go-multierror"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupLambdaEnvForTesting(t *testing.T) {
	t.Helper()

	// Suppress normal lambda log output
	logger = log.NewNopLogger()

	// Configure environment variables
	goenv.Unmarshal(goenv.EnvSet{
		"GRANTS_SOURCE_DATA_BUCKET_NAME": "test-source-data-bucket",
		"FFIS_DIGEST_EMAIL_ADDRESS":      "fake@ffis.org",
		"S3_USE_PATH_STYLE":              "true",
		"DOWNLOAD_CHUNK_LIMIT":           "10",
	}, &env)
}

func setupS3ForTesting(t *testing.T, emailBucketName string) (*s3.Client, error) {
	t.Helper()

	// Start the S3 mock server and shut it down when the test ends
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	ts := httptest.NewServer(faker.Server())
	t.Cleanup(ts.Close)

	cfg, _ := config.LoadDefaultConfig(
		context.TODO(),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("TEST", "TEST", "TESTING"),
		),
		config.WithHTTPClient(&http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		}),
		config.WithEndpointResolverWithOptions(
			aws.EndpointResolverWithOptionsFunc(func(_, _ string, _ ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: ts.URL}, nil
			}),
		),
	)

	// Create an Amazon S3 v2 client, important to use o.UsePathStyle
	// alternatively change local DNS settings, e.g., in /etc/hosts
	// to support requests to http://<bucketname>.127.0.0.1:32947/...
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
	ctx := context.TODO()
	bucketsToCreate := []string{emailBucketName, env.SourceDataBucket}
	for _, bucketName := range bucketsToCreate {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)})
		if err != nil {
			return client, err
		}
	}
	return client, nil
}

const RECEIVED_EMAIL_TEMPLATE = `From: FFIS <fake@ffis.org>
Date: Mon, 24 Apr 2023 17:42:13 -0500
Subject: Competitive Grant Update 23-17
To: <nobody@nowhere.org>


This is a test
`

func TestLambdaInvocationScenarios(t *testing.T) {
	setupLambdaEnvForTesting(t)

	sourceBucketName := "test-email-bucket"
	s3client, err := setupS3ForTesting(t, sourceBucketName)
	require.NoError(t, err)

	t.Run("Missing source object", func(t *testing.T) {
		_, err := s3client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: aws.String(sourceBucketName),
			Key:    aws.String("ses/ffis_ingest/new/test.eml"),
			Body:   bytes.NewReader([]byte(RECEIVED_EMAIL_TEMPLATE)),
		})
		require.NoError(t, err)
		err = handleS3EventWithConfig(s3client, context.TODO(), events.S3Event{
			Records: []events.S3EventRecord{
				{S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: sourceBucketName},
					Object: events.S3Object{Key: "does/not/exist"},
				}},
				{S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: sourceBucketName},
					Object: events.S3Object{Key: "ses/ffis_ingest/new/test.eml"},
				}},
			},
		})
		require.Error(t, err)
		if errs, ok := err.(*multierror.Error); ok {
			assert.Equalf(t, 1, errs.Len(),
				"Invocation accumulated an unexpected number of errors: %s", errs)
		} else {
			require.Fail(t, "Invocation error could not be interpreted as *multierror.Error")
		}

		_, err = s3client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(env.SourceDataBucket),
			Key:    aws.String("sources/2023/4/24/ffis/raw.eml"),
		})
		assert.NoError(t, err, "Expected destination object was not created")
	})

	t.Run("Context canceled during invocation", func(t *testing.T) {
		setupLambdaEnvForTesting(t)
		s3client, err := setupS3ForTesting(t, "source-bucket")
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err = handleS3EventWithConfig(s3client, ctx, events.S3Event{
			Records: []events.S3EventRecord{
				{S3: events.S3Entity{
					Bucket: events.S3Bucket{Name: "source-bucket"},
					Object: events.S3Object{Key: "does/not/matter"},
				}},
			},
		})
		require.Error(t, err)
		if errs, ok := err.(*multierror.Error); ok {
			for _, wrapped := range errs.WrappedErrors() {
				assert.ErrorIs(t, wrapped, context.Canceled,
					"context.Canceled missing in accumulated error's chain")
			}
		} else {
			require.Fail(t, "Invocation error could not be interpreted as *multierror.Error")
		}
	})
}

func TestProcessOpportunity(t *testing.T) {
	// t.Run("Destination bucket is incorrectly configured", func(t *testing.T) {
	// 	setupLambdaEnvForTesting(t)
	// 	c := mockS3ReadwriteObjectAPI{
	// 		mockHeadObjectAPI(
	// 			func(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	// 				t.Helper()
	// 				return &s3.HeadObjectOutput{}, fmt.Errorf("server error")
	// 			},
	// 		),
	// 		mockGetObjectAPI(nil),
	// 		mockPutObjectAPI(nil),
	// 	}
	// 	err := processEmail(context.TODO(), c, bytes.NewReader([]byte(RECEIVED_EMAIL_TEMPLATE)))
	// 	assert.ErrorContains(t, err, "Error determining last modified time for remote opportunity")
	// })

	t.Run("Error uploading to S3", func(t *testing.T) {
		setupLambdaEnvForTesting(t)
		s3Client := mockS3ReadwriteObjectAPI{
			mockHeadObjectAPI(
				func(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
					t.Helper()
					return nil, &awsTransport.ResponseError{
						ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{
							Response: &http.Response{StatusCode: 404},
						}},
					}
				},
			),
			mockGetObjectAPI(func(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
				t.Helper()
				require.Fail(t, "GetObject called unexpectedly")
				return nil, nil
			}),
			mockPutObjectAPI(func(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
				t.Helper()
				return nil, fmt.Errorf("some PutObject error")
			}),
		}
		err := processEmail(context.TODO(), s3Client, bytes.NewReader([]byte(RECEIVED_EMAIL_TEMPLATE)))
		assert.ErrorContains(t, err, "Error uploading S3 object to Grants source data bucket")
	})
}
