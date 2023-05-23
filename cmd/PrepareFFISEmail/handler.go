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
// for FFIS email digests delivered to an S3 bucket via SES.
// Returns an error that represents any and all errors accumulated during the invocation,
// either while handling a source object or while processing its contents; an error may indicate
// a partial or complete invocation failure.
// Returns nil when all emails are successfully processed, indicating complete success.
func handleS3EventWithConfig(s3svc *s3.Client, ctx context.Context, s3Event events.S3Event) error {
	wg := multierror.Group{}
	for _, record := range s3Event.Records {
		func(record events.S3EventRecord) {
			wg.Go(func() (err error) {
				span, ctx := tracer.StartSpanFromContext(ctx, "handle.record")
				defer span.Finish(tracer.WithError(err))
				defer func() {
					if err != nil {
						sendMetric("email.failed", 1)
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

				// data, err := io.ReadAll(resp.Body)
				// if err != nil {
				// 	log.Error(logger, "Error reading source S3 object", err)
				// 	return err
				// }

				return processEmail(ctx, s3svc, resp.Body)
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

// processEmail takes a single email, extracts the sender address and date from the header
// then checks the address against ValidFFISEmail to make sure it came from a valid source.
// If the check passes, the contents of the email are written to an object in our Grant Source
// data S3 bucket with the object key being derived from the email's sent date.
func processEmail(ctx context.Context, svc S3ReadWriteObjectAPI, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		log.Error(logger, "Error reading source S3 object", err)
		return err
	}

	emailData := strings.NewReader(string(b))
	email, err := mail.ReadMessage(emailData)
	if err != nil {
		return log.Errorf(logger, "Error parsing email data from S3", err)
	}

	address, s3ObjectKey, err := processHeader(email.Header)
	if err != nil {
		return log.Errorf(logger, "Error extracting data from email header", err)
	}

	logger := log.With(logger, "FROM", address)

	if !strings.Contains(address, env.ValidFFISEmail) {
		return log.Errorf(logger, "Origin email address does not match FFIS address", errors.New("Invalid email address"))
	}

	if err = UploadS3Object(ctx, svc, env.SourceDataBucket, s3ObjectKey, r); err != nil {
		return log.Errorf(logger, "Error uploading S3 object to Grants source data bucket", err)
	}

	log.Info(logger, "Successfully moved email")
	sendMetric("email.moved", 1)
	return nil
}

func processHeader(h mail.Header) (string, string, error) {
	mailFrom, err := mail.ParseAddress(h.Get("From"))
	if err != nil {
		return "", "", err
	}

	mailDateTime, err := mail.ParseDate(h.Get("Date"))
	if err != nil {
		return "", "", err
	}

	s3ObjectKey := fmt.Sprintf("sources/%d/%d/%d/ffis/raw.eml", mailDateTime.Year(), mailDateTime.Month(), mailDateTime.Day())

	return mailFrom.Address, s3ObjectKey, nil
}
