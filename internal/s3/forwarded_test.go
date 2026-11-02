package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// publicHost is the host the app signs for, as a proxy in front of blobfork
// (Stagd's edge) answers it.
const publicHost = "blob--pr-1.stagd.run"

// behindProxy sends requests made for publicHost to blobfork as such a proxy
// does: to blobfork's own address, with the host the app used in
// X-Forwarded-Host.
type behindProxy struct {
	to *url.URL
}

func (p behindProxy) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())

	r.Header.Set("X-Forwarded-Host", r.URL.Host)
	r.Header.Set("X-Forwarded-Proto", "https")

	r.URL.Scheme = p.to.Scheme
	r.URL.Host = p.to.Host
	r.Host = p.to.Host

	return http.DefaultTransport.RoundTrip(r)
}

// Behind a proxy that passes on a host of its own, signatures check against
// X-Forwarded-Host, but only when blobfork is told to trust it.
func TestForwardedHost(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"a.txt": "lab's"})

	ctx := t.Context()

	for _, trust := range []bool{false, true} {
		cfg := e.fork.Config
		cfg.TrustForwardedHost = trust

		fork, err := New(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}

		proxy := httptest.NewServer(fork)
		t.Cleanup(proxy.Close)

		to, _ := url.Parse(proxy.URL)
		via := &http.Client{Transport: behindProxy{to: to}}

		app := awss3.New(awss3.Options{
			BaseEndpoint: aws.String("http://" + publicHost),
			Region:       "us-east-1",
			UsePathStyle: true,
			Credentials:  aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return testCredentials, nil }),
			HTTPClient:   via,
		})

		res, err := app.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("a.txt")})

		if !trust {
			if errorCodeOf(err) != "SignatureDoesNotMatch" {
				t.Fatalf("not trusted: %v", err)
			}

			continue
		}

		if err != nil {
			t.Fatalf("trusted: %v", err)
		}

		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if string(body) != "lab's" {
			t.Fatalf("trusted read: %q", body)
		}

		// A presigned URL for the public host, as a browser would use it.
		presigned, err := awss3.NewPresignClient(app).PresignGetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("a.txt")})
		if err != nil {
			t.Fatal(err)
		}

		get, err := via.Get(presigned.URL)
		if err != nil {
			t.Fatal(err)
		}

		get.Body.Close()

		if get.StatusCode != http.StatusOK {
			t.Fatalf("presigned, trusted: %d", get.StatusCode)
		}
	}
}
