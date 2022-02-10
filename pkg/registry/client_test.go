// This Source Code Form is subject to the terms of the Apache License, Version 2.0.
// If a copy of the  APLV2 was not distributed with this file,
// You can obtain one at http://www.apache.org/licenses/LICENSE-2.0.

// Adapted from https://github.com/helm/helm/blob/v3.8.0/pkg/registry/client.go

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/errdefs"
	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry"
	_ "github.com/distribution/distribution/v3/registry/auth/htpasswd"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/phayes/freeport"
	"github.com/stretchr/testify/suite"
	"golang.org/x/crypto/bcrypt"
	helmTypes "helm.sh/helm/v3/pkg/registry"
)

var (
	testWorkspaceDir         = "registry-test"
	testHtpasswdFileBasename = "authtest.htpasswd"
	testUsername             = "myuser"
	testPassword             = "mypass"
)

type RegistryClientTestSuite struct {
	suite.Suite
	Out                     io.Writer
	DockerRegistryHost      string
	CompromisedRegistryHost string
	WorkspaceDir            string
	RegistryClient          *Client
}

func (suite *RegistryClientTestSuite) SetupSuite() {
	suite.WorkspaceDir = testWorkspaceDir
	os.RemoveAll(suite.WorkspaceDir)
	os.Mkdir(suite.WorkspaceDir, 0700)

	var out bytes.Buffer
	suite.Out = &out

	// init test client
	var err error
	suite.RegistryClient, err = NewClient(
		ClientOptDebug(true),
		ClientOptWriter(suite.Out),
	)
	suite.Nil(err, "no error creating registry client")

	// create htpasswd file (w BCrypt, which is required)
	pwBytes, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.DefaultCost)
	suite.Nil(err, "no error generating bcrypt password for test htpasswd file")
	htpasswdPath := filepath.Join(suite.WorkspaceDir, testHtpasswdFileBasename)
	err = ioutil.WriteFile(htpasswdPath, []byte(fmt.Sprintf("%s:%s\n", testUsername, string(pwBytes))), 0644)
	suite.Nil(err, "no error creating test htpasswd file")

	// Registry config
	config := &configuration.Configuration{}
	port, err := freeport.GetFreePort()
	suite.Nil(err, "no error finding free port for test registry")
	suite.DockerRegistryHost = fmt.Sprintf("localhost:%d", port)
	config.HTTP.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	config.HTTP.DrainTimeout = time.Duration(10) * time.Second
	config.Storage = map[string]configuration.Parameters{"inmemory": map[string]interface{}{}}
	config.Auth = configuration.Auth{
		"htpasswd": configuration.Parameters{
			"realm": "localhost",
			"path":  htpasswdPath,
		},
	}
	dockerRegistry, err := registry.NewRegistry(context.Background(), config)
	suite.Nil(err, "no error creating test registry")

	suite.CompromisedRegistryHost = initCompromisedRegistryTestServer()

	// Start Docker registry
	go dockerRegistry.ListenAndServe()
}

func (suite *RegistryClientTestSuite) TearDownSuite() {
	os.RemoveAll(suite.WorkspaceDir)
}

func (suite *RegistryClientTestSuite) Test_0_Login() {
	err := suite.RegistryClient.Login(suite.DockerRegistryHost,
		LoginOptBasicAuth("badverybad", "ohsobad"),
		LoginOptInsecure(false))
	suite.NotNil(err, "error logging into registry with bad credentials")

	err = suite.RegistryClient.Login(suite.DockerRegistryHost,
		LoginOptBasicAuth("badverybad", "ohsobad"),
		LoginOptInsecure(true))
	suite.NotNil(err, "error logging into registry with bad credentials, insecure mode")

	err = suite.RegistryClient.Login(suite.DockerRegistryHost,
		LoginOptBasicAuth(testUsername, testPassword),
		LoginOptInsecure(false))
	suite.Nil(err, "no error logging into registry with good credentials")

	err = suite.RegistryClient.Login(suite.DockerRegistryHost,
		LoginOptBasicAuth(testUsername, testPassword),
		LoginOptInsecure(true))
	suite.Nil(err, "no error logging into registry with good credentials, insecure mode")
}

func (suite *RegistryClientTestSuite) Test_1_Push() {
	// Bad bytes
	badContents := Contents{
		{
			Data:      []byte("hello"),
			MediaType: helmTypes.ChartLayerMediaType,
		},
	}
	ref := fmt.Sprintf("%s/testrepo/testchart:1.2.3", suite.DockerRegistryHost)
	_, err := suite.RegistryClient.Push(badContents, helmTypes.ConfigMediaType, ref)
	suite.NotNil(err, "error pushing non-chart bytes")

	// Load a test chart
	chartData, err := ioutil.ReadFile("./testdata/examplechart-0.1.0.tgz")
	suite.Nil(err, "no error loading test chart")
	meta, err := extractChartMeta(chartData)
	suite.Nil(err, "no error extracting chart meta")

	configData, err := json.Marshal(meta)
	suite.Nil(err, "error marshalling meta")

	// non-strict ref (chart name), with strict mode disabled
	contents := Contents{
		{
			Data:      configData,
			MediaType: helmTypes.ConfigMediaType,
		},
		{
			Data:      chartData,
			MediaType: helmTypes.ChartLayerMediaType,
		},
	}
	_, err = suite.RegistryClient.Push(contents, helmTypes.ConfigMediaType, ref, PushOptStrictMode(false))
	suite.Nil(err, "no error pushing non-strict ref (bad basename), with strict mode disabled")

	// basic push, good ref
	chartData, err = ioutil.ReadFile("./testdata/local-subchart-0.1.0.tgz")
	suite.Nil(err, "no error loading test chart")
	meta, err = extractChartMeta(chartData)
	suite.Nil(err, "no error extracting chart meta")

	configData, err = json.Marshal(meta)
	suite.Nil(err, "error marshalling meta")

	// non-strict ref (chart name), with strict mode disabled
	contents = Contents{
		{
			Data:      configData,
			MediaType: helmTypes.ConfigMediaType,
		},
		{
			Data:      chartData,
			MediaType: helmTypes.ChartLayerMediaType,
		},
	}

	ref = fmt.Sprintf("%s/testrepo/%s:%s", suite.DockerRegistryHost, meta.Name, meta.Version)
	_, err = suite.RegistryClient.Push(contents, helmTypes.ConfigMediaType, ref)
	suite.Nil(err, "no error pushing good ref")

	_, err = suite.RegistryClient.Pull(ref)
	suite.Nil(err, "no error pulling a simple chart")

	// push a kustomization archive
	kustomizationData, err := ioutil.ReadFile("./testdata/podinfo-kustomize-0.1.0.tgz")
	suite.Nil(err, "no error loading kustomization archive")

	contents = Contents{
		{
			Data:      nil,
			MediaType: ocispec.MediaTypeImageConfig,
		},
		{
			Data:      kustomizationData,
			MediaType: ocispec.MediaTypeImageLayerGzip,
		},
	}

	ref = fmt.Sprintf("%s/testrepo/%s:%s", suite.DockerRegistryHost, "podinfo-kustomize", "0.1.0")
	_, err = suite.RegistryClient.Push(contents, ocispec.MediaTypeImageConfig, ref)
	suite.Nil(err, "no error pushing good ref")

	_, err = suite.RegistryClient.Pull(ref)
	suite.Nil(err, "no error pulling a kustomization archive")

}

func (suite *RegistryClientTestSuite) Test_2_Pull() {
	// bad/missing ref
	ref := fmt.Sprintf("%s/testrepo/no-existy:1.2.3", suite.DockerRegistryHost)
	_, err := suite.RegistryClient.Pull(ref)
	suite.NotNil(err, "error on bad/missing ref")

	// Load test chart (to build ref pushed in previous test)
	chartData, err := ioutil.ReadFile("./testdata/local-subchart-0.1.0.tgz")
	suite.Nil(err, "no error loading test chart")
	meta, err := extractChartMeta(chartData)
	suite.Nil(err, "no error extracting chart meta")
	ref = fmt.Sprintf("%s/testrepo/%s:%s", suite.DockerRegistryHost, meta.Name, meta.Version)

	// Simple pull
	result, err := suite.RegistryClient.Pull(ref)
	suite.Nil(err, "no error pulling a simple chart")

	// Validate the output
	suite.Equal(ref, result.Ref)
	suite.Equal(int64(352), result.Manifest.Size)
	suite.Equal(int64(105), result.Config.Size)
	suite.Equal(int64(259), result.Layers[0].Size)
	suite.Equal(
		"sha256:7cfc000b6a983861357c02832022aa907d5fa614730f3e4a2daf8872f00c32c7",
		result.Manifest.Digest)
	suite.Equal(
		"sha256:21bbe4e443d075d4b99b610edabbdb67f1f5ae12b821a2b3dae01b53deec81d4",
		result.Config.Digest)
	suite.Equal(
		"sha256:d2ab7310924fa816445fa7dec0a829b005ece8592219e9714c9572ec47847c9a",
		result.Layers[0].Digest)
	suite.Equal("{\"schemaVersion\":2,\"config\":{\"mediaType\":\"application/vnd.cncf.helm.config.v1+json\",\"digest\":\"sha256:21bbe4e443d075d4b99b610edabbdb67f1f5ae12b821a2b3dae01b53deec81d4\",\"size\":105},\"layers\":[{\"mediaType\":\"application/vnd.cncf.helm.chart.content.v1.tar+gzip\",\"digest\":\"sha256:d2ab7310924fa816445fa7dec0a829b005ece8592219e9714c9572ec47847c9a\",\"size\":259}]}",
		string(result.Manifest.Data))
	suite.Equal("{\"name\":\"local-subchart\",\"version\":\"0.1.0\",\"description\":\"A Helm chart for Kubernetes\",\"apiVersion\":\"v1\"}",
		string(result.Config.Data))
	suite.Equal(chartData, result.Layers[0].Data)
}

func (suite *RegistryClientTestSuite) Test_3_Tags() {

	// Load test chart (to build ref pushed in previous test)
	chartData, err := ioutil.ReadFile("./testdata/local-subchart-0.1.0.tgz")
	suite.Nil(err, "no error loading test chart")
	meta, err := extractChartMeta(chartData)
	suite.Nil(err, "no error extracting chart meta")
	ref := fmt.Sprintf("%s/testrepo/%s", suite.DockerRegistryHost, meta.Name)

	// Query for tags and validate length
	tags, err := suite.RegistryClient.Tags(ref)
	suite.Nil(err, "no error retrieving tags")
	suite.Equal(1, len(tags))

}

func (suite *RegistryClientTestSuite) Test_4_Logout() {
	err := suite.RegistryClient.Logout("this-host-aint-real:5000")
	suite.NotNil(err, "error logging out of registry that has no entry")

	err = suite.RegistryClient.Logout(suite.DockerRegistryHost)
	suite.Nil(err, "no error logging out of registry")
}

func (suite *RegistryClientTestSuite) Test_5_ManInTheMiddle() {
	ref := fmt.Sprintf("%s/testrepo/supposedlysafechart:9.9.9", suite.CompromisedRegistryHost)

	// returns content that does not match the expected digest
	_, err := suite.RegistryClient.Pull(ref)
	suite.NotNil(err)
	suite.True(errdefs.IsFailedPrecondition(err))
}

func TestRegistryClientTestSuite(t *testing.T) {
	suite.Run(t, new(RegistryClientTestSuite))
}

func initCompromisedRegistryTestServer() string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "manifests") {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.WriteHeader(200)

			// layers[0] is the blob []byte("a")
			w.Write([]byte(
				fmt.Sprintf(`{ "schemaVersion": 2, "config": {
    "mediaType": "%s",
    "digest": "sha256:a705ee2789ab50a5ba20930f246dbd5cc01ff9712825bb98f57ee8414377f133",
    "size": 181
  },
  "layers": [
    {
      "mediaType": "%s",
      "digest": "sha256:ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb",
      "size": 1
    }
  ]
}`, helmTypes.ConfigMediaType, helmTypes.ChartLayerMediaType)))
		} else if r.URL.Path == "/v2/testrepo/supposedlysafechart/blobs/sha256:a705ee2789ab50a5ba20930f246dbd5cc01ff9712825bb98f57ee8414377f133" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte("{\"name\":\"mychart\",\"version\":\"0.1.0\",\"description\":\"A Helm chart for Kubernetes\\n" +
				"an 'application' or a 'library' chart.\",\"apiVersion\":\"v2\",\"appVersion\":\"1.16.0\",\"type\":" +
				"\"application\"}"))
		} else if r.URL.Path == "/v2/testrepo/supposedlysafechart/blobs/sha256:ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb" {
			w.Header().Set("Content-Type", helmTypes.ChartLayerMediaType)
			w.WriteHeader(200)
			w.Write([]byte("b"))
		} else {
			w.WriteHeader(500)
		}
	}))

	u, _ := url.Parse(s.URL)
	return fmt.Sprintf("localhost:%s", u.Port())
}
