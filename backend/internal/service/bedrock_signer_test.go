package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBedrockSignerFromAccount_DefaultRegion(t *testing.T) {
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeBedrock,
		Credentials: map[string]any{
			"aws_access_key_id":     "test-akid",
			"aws_secret_access_key": "test-secret",
		},
	}

	signer, err := NewBedrockSignerFromAccount(account)
	require.NoError(t, err)
	require.NotNil(t, signer)
	assert.Equal(t, defaultBedrockRegion, signer.region)
}

func TestFilterBetaTokens(t *testing.T) {
	tokens := []string{"interleaved-thinking-2025-05-14", "tool-search-tool-2025-10-19"}
	filterSet := map[string]struct{}{
		"tool-search-tool-2025-10-19": {},
	}

	assert.Equal(t, []string{"interleaved-thinking-2025-05-14"}, filterBetaTokens(tokens, filterSet))
	assert.Equal(t, tokens, filterBetaTokens(tokens, nil))
	assert.Nil(t, filterBetaTokens(nil, filterSet))
}

func TestBedrockSignerRoleCredentialsSharedAndRefreshed(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>assumed-key</AccessKeyId><SecretAccessKey>assumed-secret</SecretAccessKey><SessionToken>token-%d</SessionToken><Expiration>%s</Expiration></Credentials><AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/test/session</Arn><AssumedRoleId>test:session</AssumedRoleId></AssumedRoleUser></AssumeRoleResult><ResponseMetadata><RequestId>test</RequestId></ResponseMetadata></AssumeRoleResponse>`, n, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	t.Setenv("AWS_ENDPOINT_URL_STS", server.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "source-test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "source-test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	account := &Account{Credentials: map[string]any{"auth_mode": "iam_role", "aws_role_arn": "arn:aws:iam::123456789012:role/test", "aws_region": "us-east-1"}}
	key := bedrockRoleProviderKey{"arn:aws:iam::123456789012:role/test", "us-east-1", ""}
	bedrockRoleProviders.Delete(key)
	t.Cleanup(func() { bedrockRoleProviders.Delete(key) })
	sign := func() (*http.Request, error) {
		signer, err := newBedrockSignerFromAccount(context.Background(), account)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/test/invoke", nil)
		if err != nil {
			return nil, err
		}
		return req, signer.SignRequest(context.Background(), req, nil)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := sign()
			if err != nil {
				failures <- err
				return
			}
			if req.Header.Get("X-Amz-Security-Token") != "token-1" {
				failures <- fmt.Errorf("unexpected token: %q", req.Header.Get("X-Amz-Security-Token"))
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, calls.Load(), "requests must share one STS session")
	cached, ok := bedrockRoleProviders.Load(key)
	require.True(t, ok)
	cached.(*aws.CredentialsCache).Invalidate()
	req, err := sign()
	require.NoError(t, err)
	require.Equal(t, "token-2", req.Header.Get("X-Amz-Security-Token"))
	require.Contains(t, req.Header.Get("Authorization"), "Credential=assumed-key/")
	require.EqualValues(t, 2, calls.Load(), "invalidated sessions must refresh")
}

func TestBedrockSignerRoleRequiresARN(t *testing.T) {
	_, err := newBedrockSignerFromAccount(context.Background(), &Account{Credentials: map[string]any{"auth_mode": "iam_role"}})
	require.ErrorContains(t, err, "aws_role_arn is required")
}

func TestBedrockSignerCredentialFailure(t *testing.T) {
	signer := &BedrockSigner{provider: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, fmt.Errorf("STS unavailable")
	}), region: "us-east-1", signer: v4.NewSigner()}
	req, err := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/test/invoke", nil)
	require.NoError(t, err)
	require.ErrorContains(t, signer.SignRequest(context.Background(), req, nil), "STS unavailable")
	require.Empty(t, req.Header.Get("Authorization"))
}
