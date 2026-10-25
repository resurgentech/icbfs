// Package storeconfig is how every part of icbfs says which object store
// to use and how to reach it: the mounts' flags, icbfs-sync's and the
// drive API's configuration, and what the logging daemon hands its mounts.
// pkg/icbfs.Open opens one.
package storeconfig

import (
	"errors"
	"fmt"
)

// Config is a store: Kind names it, and the section of that name says how
// to reach it.
type Config struct {
	Kind  string `json:"kind,omitempty"` // KindS3 (the default) or KindAzure
	S3    S3     `json:"s3"`
	Azure Azure  `json:"azure"`
}

const (
	KindS3    = "s3"    // AWS S3, MinIO, or another S3-API store
	KindAzure = "azure" // Azure Blob Storage
)

// S3 is a bucket over the S3 API, with versioning on.
type S3 struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey,omitempty"`
}

// Azure is a container in an Azure Blob Storage account with blob
// versioning on. Endpoint is the account's blob service URL, by default
// https://ACCOUNT.blob.core.windows.net/ (Azurite and sovereign clouds
// differ).
type Azure struct {
	Account   string `json:"account"`
	Container string `json:"container"`
	Key       string `json:"key,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// ServiceURL is the account's blob service URL.
func (a Azure) ServiceURL() string {
	if a.Endpoint != "" {
		return a.Endpoint
	}
	return "https://" + a.Account + ".blob.core.windows.net/"
}

// ErrNoKeys is Check's answer when the store's credentials are missing.
var ErrNoKeys = errors.New("no store credentials")

// Check reports whether c names a known kind and has its credentials.
func (c Config) Check() error {
	switch c.Kind {
	case "", KindS3:
		if c.S3.Endpoint == "" || c.S3.Bucket == "" {
			return errors.New("an S3 store needs an endpoint and a bucket")
		}
		if c.S3.AccessKey == "" || c.S3.SecretKey == "" {
			return fmt.Errorf("%w: an S3 store needs an access key and a secret key", ErrNoKeys)
		}
	case KindAzure:
		if c.Azure.Account == "" || c.Azure.Container == "" {
			return errors.New("an Azure store needs an account and a container")
		}
		if c.Azure.Key == "" {
			return fmt.Errorf("%w: an Azure store needs the account's key", ErrNoKeys)
		}
	default:
		return fmt.Errorf("store kind %q: %s or %s", c.Kind, KindS3, KindAzure)
	}
	return nil
}

// Secret is the store's secret, whichever kind it is, and SetSecret sets
// it: for configurations that keep it apart (icbfs-sync's, encrypted).
func (c Config) Secret() string {
	if c.Kind == KindAzure {
		return c.Azure.Key
	}
	return c.S3.SecretKey
}

func (c *Config) SetSecret(secret string) {
	if c.Kind == KindAzure {
		c.Azure.Key = secret
	} else {
		c.S3.SecretKey = secret
	}
}

// String says which store, for logs: never a secret.
func (c Config) String() string {
	if c.Kind == KindAzure {
		return fmt.Sprintf("Azure container %s in %s", c.Azure.Container, c.Azure.ServiceURL())
	}
	return fmt.Sprintf("bucket %s via %s", c.S3.Bucket, c.S3.Endpoint)
}
