// Copyright 2021 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sigv4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	signer "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

var sigv4HeaderDenylist = []string{
	"uber-trace-id",
}

type sigV4RoundTripper struct {
	region string
	next   http.RoundTripper
	pool   sync.Pool
	creds  *aws.CredentialsCache

	signer *signer.Signer
}

var ctx context.Context = context.TODO()

// NewSigV4RoundTripper returns a new http.RoundTripper that will sign requests
// using Amazon's Signature Verification V4 signing procedure. The request will
// then be handed off to the next RoundTripper provided by next. If next is nil,
// http.DefaultTransport will be used.
//
// Credentials for signing are retrieved using the the default AWS credential
// chain. If credentials cannot be found, an error will be returned.
func NewSigV4RoundTripper(cfg *SigV4Config, next http.RoundTripper) (http.RoundTripper, error) {
	if next == nil {
		next = http.DefaultTransport
	}

	awsConfig := []func(*config.LoadOptions) error{}

	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		awsConfig = append(awsConfig, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, string(cfg.SecretKey), ""),
		))
	}

	if cfg.UseFIPSSTSEndpoint {
		awsConfig = append(awsConfig, config.WithUseFIPSEndpoint(aws.FIPSEndpointStateEnabled))
	} else {
		awsConfig = append(awsConfig, config.WithUseFIPSEndpoint(aws.FIPSEndpointStateDisabled))
	}

	if cfg.Region != "" {
		awsConfig = append(awsConfig, config.WithRegion(cfg.Region))
	}

	if cfg.Profile != "" {
		awsConfig = append(awsConfig, config.WithSharedConfigProfile(cfg.Profile))
	}

	awscfg, err := config.LoadDefaultConfig(
		ctx,
		awsConfig...,
	)
	if err != nil {
		return nil, fmt.Errorf("could not create new AWS session: %w", err)
	}
	if _, err := awscfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("could not get SigV4 credentials: %w", err)
	}
	if awscfg.Region == "" {
		return nil, fmt.Errorf("region not configured in sigv4 or in default credentials chain")
	}

	if cfg.RoleARN != "" {
		awscfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(awscfg), cfg.RoleARN))
	}

	rt := &sigV4RoundTripper{
		region: cfg.Region,
		next:   next,
		creds:  aws.NewCredentialsCache(awscfg.Credentials),
	}
	rt.pool.New = rt.newBuf
	return rt, nil
}

func (rt *sigV4RoundTripper) newBuf() interface{} {
	return bytes.NewBuffer(make([]byte, 0, 1024))
}

func (rt *sigV4RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// // rt.signer.Sign needs a seekable body, so we replace the body with a
	// // buffered reader filled with the contents of original body.
	// buf := rt.pool.Get().(*bytes.Buffer)
	// defer func() {
	// 	buf.Reset()
	// 	rt.pool.Put(buf)
	// }()

	// if req.Body != nil {
	// 	if _, err := io.Copy(buf, req.Body); err != nil {
	// 		return nil, err
	// 	}
	// 	// Close the original body since we don't need it anymore.
	// 	_ = req.Body.Close()
	// }

	// // Ensure our seeker is back at the start of the buffer once we return.
	// var seeker io.ReadSeeker = bytes.NewReader(buf.Bytes())
	// defer func() {
	// 	_, _ = seeker.Seek(0, io.SeekStart)
	// }()
	// req.Body = io.NopCloser(seeker)

	defer req.Body.Close()
	// Clean path like documented in AWS documentation.
	// https://docs.aws.amazon.com/general/latest/gr/sigv4-create-canonical-request.html
	req.URL.Path = path.Clean(req.URL.Path)

	// Clone the request and trim out headers that we don't want to sign.
	signReq := req.Clone(req.Context())
	for _, header := range sigv4HeaderDenylist {
		signReq.Header.Del(header)
	}
	creds, err := rt.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("error retrieving credentials: %w", err)
	}

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	hash := sha256.Sum256(bodyBytes)
	strHash := hex.EncodeToString(hash[:])
	err = rt.signer.SignHTTP(
		ctx,
		creds,
		signReq,
		strHash,
		"aps",
		rt.region,
		time.Now().UTC(),
	)

	// headers, err := rt.signer.Sign(signReq, seeker, "aps", rt.region, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("failed to sign request: %w", err)
	}

	// // Copy over signed headers. Authorization header is not returned by
	// // rt.signer.Sign and needs to be copied separately.
	// for k, v := range headers {
	// 	req.Header[textproto.CanonicalMIMEHeaderKey(k)] = v
	// }
	// req.Header.Set("Authorization", signReq.Header.Get("Authorization"))

	return rt.next.RoundTrip(req)
}
