package storeconfig

import (
	"encoding/json"
	"testing"
)

func TestUnmarshal(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want Config
	}{
		"s3": {in: `{"kind":"s3","s3":{"endpoint":"http://e","bucket":"b","accessKey":"a","secretKey":"s"}}`,
			want: Config{Kind: KindS3, S3: S3{Endpoint: "http://e", Bucket: "b", AccessKey: "a", SecretKey: "s"}}},
		"azure": {in: `{"kind":"azure","azure":{"account":"acct","container":"c","key":"k"}}`,
			want: Config{Kind: KindAzure, Azure: Azure{Account: "acct", Container: "c", Key: "k"}}},
	} {
		var c Config
		err := json.Unmarshal([]byte(tc.in), &c)
		if err != nil || c != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", name, c, err, tc.want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	c := Config{Kind: KindAzure, Azure: Azure{Account: "acct", Container: "c", Key: "k", Endpoint: "http://127.0.0.1:10000/acct/"}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil || got != c {
		t.Fatalf("%s → %+v, %v", b, got, err)
	}
}

func TestCheck(t *testing.T) {
	ok := []Config{
		{S3: S3{Endpoint: "http://e", Bucket: "b", AccessKey: "a", SecretKey: "s"}},
		{Kind: KindAzure, Azure: Azure{Account: "acct", Container: "c", Key: "k"}},
	}
	for _, c := range ok {
		if err := c.Check(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := []Config{
		{S3: S3{Endpoint: "http://e", Bucket: "b", AccessKey: "a"}},
		{Kind: KindAzure, Azure: Azure{Account: "acct", Key: "k"}},
		{Kind: KindAzure, Azure: Azure{Account: "acct", Container: "c"}},
		{Kind: "gcs"},
	}
	for _, c := range bad {
		if c.Check() == nil {
			t.Errorf("%+v: no error", c)
		}
	}
}

func TestServiceURL(t *testing.T) {
	if got := (Azure{Account: "acct"}).ServiceURL(); got != "https://acct.blob.core.windows.net/" {
		t.Error(got)
	}
}
