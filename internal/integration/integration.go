// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/exposure-notifications-server/internal/authorizedapp"
	"github.com/google/exposure-notifications-server/internal/cleanup"
	"github.com/google/exposure-notifications-server/internal/database"
	"github.com/google/exposure-notifications-server/internal/export"
	"github.com/google/exposure-notifications-server/internal/federationin"
	"github.com/google/exposure-notifications-server/internal/publish"
	"github.com/google/exposure-notifications-server/internal/server"
	"github.com/google/exposure-notifications-server/internal/serverenv"
	"github.com/google/exposure-notifications-server/internal/storage"
	"github.com/google/exposure-notifications-server/pkg/keys"
	"github.com/google/exposure-notifications-server/pkg/secrets"

	authorizedappmodel "github.com/google/exposure-notifications-server/internal/authorizedapp/model"
	exportdatabase "github.com/google/exposure-notifications-server/internal/export/database"
	exportmodel "github.com/google/exposure-notifications-server/internal/export/model"
)

const (
	exportDir = "my-bucket"
)

// NewTestServer sets up clients used for integration tests
func NewTestServer(tb testing.TB, ctx context.Context, exportPeriod time.Duration) (*serverenv.ServerEnv, *EnServerClient, *database.DB) {
	env, client := testServer(tb)
	db := env.Database()
	enClient := &EnServerClient{client: client}

	// Create an authorized app.
	startAuthorizedApp(ctx, env, tb)

	// Create a signature info.
	createSignatureInfo(ctx, db, exportPeriod, tb)

	return env, enClient, db
}

// testServer sets up mocked local servers for running tests
func testServer(tb testing.TB) (*serverenv.ServerEnv, *http.Client) {
	tb.Helper()

	ctx := context.Background()

	aa, err := authorizedapp.NewMemoryProvider(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}

	bs, err := storage.NewMemory(ctx)
	if err != nil {
		tb.Fatal(err)
	}

	db := database.NewTestDatabase(tb)

	km, err := keys.NewNoop(ctx)
	if err != nil {
		tb.Fatal(err)
	}

	sm, err := secrets.NewNoop(ctx)
	if err != nil {
		tb.Fatal(err)
	}

	env := serverenv.New(ctx,
		serverenv.WithAuthorizedAppProvider(aa),
		serverenv.WithBlobStorage(bs),
		serverenv.WithDatabase(db),
		serverenv.WithKeyManager(km),
		serverenv.WithSecretManager(sm),
	)
	// Note: don't call env.Cleanup() because the database helper closes the
	// connection for us.

	mux := http.NewServeMux()

	// Cleanup export
	cleanupExportConfig := &cleanup.Config{
		Timeout: 10 * time.Minute,
		TTL:     336 * time.Hour,
	}

	cleanupExportHandler, err := cleanup.NewExportHandler(cleanupExportConfig, env)
	if err != nil {
		tb.Fatal(err)
	}
	mux.Handle("/cleanup-export", cleanupExportHandler)

	// Cleanup exposure
	cleanupExposureConfig := &cleanup.Config{
		Timeout: 10 * time.Minute,
		TTL:     336 * time.Hour,
	}

	cleanupExposureHandler, err := cleanup.NewExposureHandler(cleanupExposureConfig, env)
	if err != nil {
		tb.Fatal(err)
	}
	mux.Handle("/cleanup-exposure", cleanupExposureHandler)

	// Export
	exportConfig := &export.Config{
		CreateTimeout:  10 * time.Second,
		WorkerTimeout:  10 * time.Second,
		MinRecords:     1,
		PaddingRange:   1,
		MaxRecords:     10000,
		TruncateWindow: 1 * time.Second,
		MinWindowAge:   1 * time.Second,
		TTL:            336 * time.Hour,
	}

	exportServer, err := export.NewServer(exportConfig, env)
	if err != nil {
		tb.Fatal(err)
	}
	mux.Handle("/export/", http.StripPrefix("/export", exportServer.Routes(ctx)))

	// Federation
	federationInConfig := &federationin.Config{
		Timeout:        10 * time.Minute,
		TruncateWindow: 1 * time.Hour,
	}

	mux.Handle("/federation-in", federationin.NewHandler(env, federationInConfig))

	// Federation out
	// TODO: this is a grpc listener and requires a lot of setup.

	// Publish
	publishConfig := &publish.Config{
		MaxKeysOnPublish:         15,
		MaxSameStartIntervalKeys: 2,
		MaxIntervalAge:           360 * time.Hour,
		CreatedAtTruncateWindow:  1 * time.Second,
		ReleaseSameDayKeys:       true,
	}

	publishHandler, err := publish.NewHandler(ctx, publishConfig, env)
	if err != nil {
		tb.Fatal(err)
	}
	mux.Handle("/publish", publishHandler)

	srv, err := server.New("")
	if err != nil {
		tb.Fatal(err)
	}

	// Stop the server on cleanup
	stopCtx, stop := context.WithCancel(ctx)
	tb.Cleanup(stop)

	// Start the server
	go func() {
		if err := srv.ServeHTTPHandler(stopCtx, mux); err != nil {
			tb.Error(err)
		}
	}()

	// Create a client
	client := testClient(tb, srv)

	return env, client
}

type prefixRoundTripper struct {
	addr string
	rt   http.RoundTripper
}

func (p *prefixRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	if u.Host == "" {
		u.Host = p.addr
	}

	return p.rt.RoundTrip(r)
}

func testClient(tb testing.TB, srv *server.Server) *http.Client {
	prt := &prefixRoundTripper{
		addr: srv.Addr(),
		rt:   http.DefaultTransport,
	}

	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: prt,
	}
}

func startAuthorizedApp(ctx context.Context, env *serverenv.ServerEnv, tb testing.TB) {
	aa := env.AuthorizedAppProvider()
	if err := aa.Add(ctx, &authorizedappmodel.AuthorizedApp{
		AppPackageName: "com.example.app",
		AllowedRegions: map[string]struct{}{
			"TEST": {},
		},
		AllowedHealthAuthorityIDs: map[int64]struct{}{
			12345: {},
		},

		// TODO: hook up verification, and disable
		BypassHealthAuthorityVerification: true,
	}); err != nil {
		tb.Fatal(err)
	}
}

func createSignatureInfo(ctx context.Context, db *database.DB, exportPeriod time.Duration, tb testing.TB) {
	si := &exportmodel.SignatureInfo{
		SigningKey:        "signer",
		SigningKeyVersion: "v1",
		SigningKeyID:      "US",
	}
	if err := exportdatabase.New(db).AddSignatureInfo(ctx, si); err != nil {
		tb.Fatal(err)
	}

	// Create an export config.
	ec := &exportmodel.ExportConfig{
		BucketName:       exportDir,
		Period:           exportPeriod,
		OutputRegion:     "TEST",
		From:             time.Now().Add(-2 * time.Second),
		Thru:             time.Now().Add(1 * time.Hour),
		SignatureInfoIDs: []int64{},
	}
	if err := exportdatabase.New(db).AddExportConfig(ctx, ec); err != nil {
		tb.Fatal(err)
	}
}
