package kms

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// Client wraps the AWS KMS SDK to perform decryption operations.
type Client struct {
	kms *kms.Client
}

// New creates a KMS Client. If localStackEndpoint is non-empty, the client
// targets that endpoint with dummy credentials (for local development).
// Otherwise it uses the AWS default credential chain (IAM Roles in production).
func New(ctx context.Context, region, localStackEndpoint string) (*Client, error) {
	var opts []func(*config.LoadOptions) error
	opts = append(opts, config.WithRegion(region))

	if localStackEndpoint != "" {
		opts = append(opts,
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
		)
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("kms: load aws config: %w", err)
	}

	var kmsOpts []func(*kms.Options)
	if localStackEndpoint != "" {
		kmsOpts = append(kmsOpts, func(o *kms.Options) {
			o.BaseEndpoint = aws.String(localStackEndpoint)
		})
	}

	return &Client{
		kms: kms.NewFromConfig(cfg, kmsOpts...),
	}, nil
}

// Decrypt sends the ciphertext blob to KMS and returns the decrypted plaintext bytes.
// The caller is responsible for securing the returned bytes (e.g. mlock).
func (c *Client) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	out, err := c.kms.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: ciphertext,
	})
	if err != nil {
		return nil, fmt.Errorf("kms: decrypt: %w", err)
	}
	return out.Plaintext, nil
}
