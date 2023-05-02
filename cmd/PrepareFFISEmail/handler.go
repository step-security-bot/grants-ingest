package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/hashicorp/go-multierror"
	"github.com/usdigitalresponse/grants-ingest/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

const (
	MB = int64(1024 * 1024)
)

// handleS3Event handles events representing S3 bucket notifications of type "ObjectCreated:*"
// for XML DB extracts saved from Grants.gov and split into separate files via the SplitGrantsGovXMLDB Lambda.
// The XML data from the source S3 object provided represents an individual grant opportunity.
// Returns an error that represents any and all errors accumulated during the invocation,
// either while handling a source object or while processing its contents; an error may indicate
// a partial or complete invocation failure.
// Returns nil when all grant opportunities are successfully processed from all source records,
// indicating complete success.
func handleS3EventWithConfig(s3svc *s3.Client, ctx context.Context, s3Event events.S3Event) error {
	wg := multierror.Group{}
	for _, record := range s3Event.Records {
		func(record events.S3EventRecord) {
			wg.Go(func() (err error) {
				span, ctx := tracer.StartSpanFromContext(ctx, "handle.record")
				defer span.Finish(tracer.WithError(err))
				defer func() {
					if err != nil {
						sendMetric("opportunity.failed", 1)
					}
				}()

				sourceBucket := record.S3.Bucket.Name
				sourceKey := record.S3.Object.Key
				logger := log.With(logger, "event_name", record.EventName,
					"source_bucket", sourceBucket, "source_object_key", sourceKey)

				resp, err := s3svc.GetObject(ctx, &s3.GetObjectInput{
					Bucket: aws.String(sourceBucket),
					Key:    aws.String(sourceKey),
				})
				if err != nil {
					log.Error(logger, "Error getting source S3 object", err)
					return err
				}

				data, err := io.ReadAll(resp.Body)
				if err != nil {
					log.Error(logger, "Error reading source opportunity from S3", err)
					return err
				}

				return processEmail(ctx, s3svc, data)
			})
		}(record)
	}

	errs := wg.Wait()
	if err := errs.ErrorOrNil(); err != nil {
		log.Warn(logger, "Failures occurred during invocation; check logs for details",
			"count_errors", errs.Len(),
			"count_s3_events", len(s3Event.Records))
		return err
	}
	return nil
}

// processOpportunity takes a single opportunity and uploads an XML representation of the
// opportunity to its configured DynamoDB table.
func processEmail(ctx context.Context, svc *s3.Client, b []byte) error {
	emailData := strings.NewReader(string(b))
	email, err := mail.ReadMessage(emailData)
	if err != nil {
		return log.Errorf(logger, "Error parsing email data from S3", err)
	}

	header := email.Header
	mailFrom, err := mail.ParseAddress(header.Get("From"))
	if err != nil {
		return log.Errorf(logger, "Error parsing email address", err)
	}

	mailDate := header.Get("Date")
	logger := log.With(logger, "FROM", mailFrom.Address, "DATE", mailDate)

	if !strings.Contains(mailFrom.Address, env.ValidFFISEmail) {
		return log.Errorf(logger, "Origin email address does not match FFIS address", errors.New("Invalid email address"))
	}

	mailDateTime, err := mail.ParseDate(mailDate)
	if err != nil {
		return log.Errorf(logger, "Error parsing date for S3 object key", err)
	}
	s3ObjectKey := fmt.Sprintf("sources/%d/%d/%d/ffis/raw.eml", mailDateTime.Year(), mailDateTime.Month(), mailDateTime.Day())

	if err = UploadS3Object(ctx, svc, env.SourceDataBucket, s3ObjectKey, b); err != nil {
		return log.Errorf(logger, "Error uploading prepared grant opportunity to DynamoDB", err)
	}

	log.Info(logger, "Successfully moved email")
	sendMetric("email.moved", 1)
	return nil
}
