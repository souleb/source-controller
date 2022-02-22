// This Source Code Form is subject to the terms of the Apache License, Version 2.0.
// If a copy of the  APLV2 was not distributed with this file,
// You can obtain one at http://www.apache.org/licenses/LICENSE-2.0.

// Adapted from https://github.com/helm/helm/blob/v3.8.0/pkg/registry/client.go

package registry

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/containerd/containerd/remotes"
	distributionSchema "github.com/distribution/distribution/manifest/schema2"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	helmTypes "helm.sh/helm/v3/pkg/registry"
	"oras.land/oras-go/pkg/auth"
	dockerauth "oras.land/oras-go/pkg/auth/docker"
	"oras.land/oras-go/pkg/content"
	"oras.land/oras-go/pkg/oras"
	"oras.land/oras-go/pkg/registry"
	registryremote "oras.land/oras-go/pkg/registry/remote"
	registryauth "oras.land/oras-go/pkg/registry/remote/auth"
)

// Client works with OCI-compliant registries
type Client struct {
	debug              bool
	out                io.Writer
	authorizer         auth.Client
	registryAuthorizer *registryauth.Client
	resolver           remotes.Resolver
}

// ClientOption allows specifying various settings configurable by the user for overriding the defaults
// used when creating a new default client
type ClientOption func(*Client)

// NewClient returns a new registry client with config
func NewClient(options ...ClientOption) (*Client, error) {
	client := &Client{
		out: ioutil.Discard,
	}
	for _, option := range options {
		option(client)
	}

	if client.authorizer == nil {
		authClient, err := dockerauth.NewClientWithDockerFallback()
		if err != nil {
			return nil, err
		}

		client.authorizer = authClient
	}
	if client.resolver == nil {
		headers := http.Header{}
		headers.Set("User-Agent", "source-controller")
		opts := []auth.ResolverOption{auth.WithResolverHeaders(headers)}
		resolver, err := client.authorizer.ResolverWithOpts(opts...)
		if err != nil {
			return nil, err
		}
		client.resolver = resolver
	}

	if client.registryAuthorizer == nil {
		client.registryAuthorizer = &registryauth.Client{
			Header: http.Header{
				"User-Agent": {"source-controller"},
			},
			Cache: registryauth.DefaultCache,
			Credential: func(ctx context.Context, reg string) (registryauth.Credential, error) {
				dockerClient, ok := client.authorizer.(*dockerauth.Client)
				if !ok {
					return registryauth.EmptyCredential, errors.New("unable to obtain docker client")
				}

				username, password, err := dockerClient.Credential(reg)
				if err != nil {
					return registryauth.EmptyCredential, errors.New("unable to retrieve credentials")
				}

				// A blank returned username and password value is a bearer token
				if username == "" && password != "" {
					return registryauth.Credential{
						RefreshToken: password,
					}, nil
				}

				return registryauth.Credential{
					Username: username,
					Password: password,
				}, nil
			},
		}
	}

	return client, nil
}

// ClientOptDebug returns a function that sets the debug setting on client options set
func ClientOptDebug(debug bool) ClientOption {
	return func(client *Client) {
		client.debug = debug
	}
}

// ClientOptWriter returns a function that sets the writer setting on client options set
func ClientOptWriter(out io.Writer) ClientOption {
	return func(client *Client) {
		client.out = out
	}
}

// LoginOption allows specifying various settings on login
type LoginOption func(*loginOperation)

type loginOperation struct {
	username string
	password string
	insecure bool
}

// Login logs into a registry
func (c *Client) Login(host string, options ...LoginOption) error {
	operation := &loginOperation{}
	for _, option := range options {
		option(operation)
	}
	authorizerLoginOpts := []auth.LoginOption{
		auth.WithLoginContext(ctx(c.out, c.debug)),
		auth.WithLoginHostname(host),
		auth.WithLoginUsername(operation.username),
		auth.WithLoginSecret(operation.password),
		auth.WithLoginUserAgent("source-controller"),
	}
	if operation.insecure {
		authorizerLoginOpts = append(authorizerLoginOpts, auth.WithLoginInsecure())
	}
	if err := c.authorizer.LoginWithOpts(authorizerLoginOpts...); err != nil {
		return fmt.Errorf("login failed: %s", err)
	}
	fmt.Fprintln(c.out, "Login Succeeded")
	return nil
}

// LoginOptBasicAuth returns a function that sets the username/password settings on login
func LoginOptBasicAuth(username string, password string) LoginOption {
	return func(operation *loginOperation) {
		operation.username = username
		operation.password = password
	}
}

// LoginOptInsecure returns a function that sets the insecure setting on login
func LoginOptInsecure(insecure bool) LoginOption {
	return func(operation *loginOperation) {
		operation.insecure = insecure
	}
}

type (
	// LogoutOption allows specifying various settings on logout
	LogoutOption func(*logoutOperation)

	logoutOperation struct{}
)

// Logout logs out of a registry
func (c *Client) Logout(host string, opts ...LogoutOption) error {
	operation := &logoutOperation{}
	for _, opt := range opts {
		opt(operation)
	}
	if err := c.authorizer.Logout(ctx(c.out, c.debug), host); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Removing login credentials for %s\n", host)
	return nil
}

// PullResult is the result returned upon successful pull.
type PullResult struct {
	Manifest *descriptorPullSummary   `json:"manifest"`
	Config   *descriptorPullSummary   `json:"config"`
	Layers   []*descriptorPullSummary `json:"layers"`
	Ref      string                   `json:"ref"`
}

type descriptorPullSummary struct {
	MediaType string `json:"mediaType"`
	Data      []byte `json:"-"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

func (c *Client) PullManifest(ref string) (ocispec.Descriptor, error) {
	parsedRef, err := registry.ParseReference(ref)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	memoryStore := content.NewMemory()
	allowedMediaTypes := []string{
		helmTypes.ConfigMediaType,
		helmTypes.ChartLayerMediaType,
	}

	registryStore := content.Registry{Resolver: c.resolver}

	manifest, err := oras.Copy(ctx(c.out, c.debug), registryStore, parsedRef.String(), memoryStore, "",
		oras.WithPullEmptyNameAllowed(),
		oras.WithAllowedMediaTypes(allowedMediaTypes))
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	return manifest, nil

}

// Pull downloads an artifact from a registry
func (c *Client) Pull(ref string) (*PullResult, error) {
	parsedRef, err := registry.ParseReference(ref)
	if err != nil {
		return nil, err
	}

	memoryStore := content.NewMemory()
	allowedMediaTypes := []string{
		ocispec.MediaTypeImageConfig,
		ocispec.MediaTypeImageLayerGzip,
		helmTypes.ConfigMediaType,
		helmTypes.ChartLayerMediaType,
		helmTypes.ProvLayerMediaType,
		distributionSchema.MediaTypeManifest,
		distributionSchema.MediaTypeLayer,
		distributionSchema.MediaTypeImageConfig,
	}

	var layers []ocispec.Descriptor
	registryStore := content.Registry{Resolver: c.resolver}

	manifest, err := oras.Copy(ctx(c.out, c.debug), registryStore, parsedRef.String(), memoryStore, "",
		oras.WithPullEmptyNameAllowed(),
		oras.WithAllowedMediaTypes(allowedMediaTypes),
		oras.WithAdditionalCachedMediaTypes(distributionSchema.MediaTypeManifest),
		oras.WithLayerDescriptors(func(l []ocispec.Descriptor) {
			layers = l
		}))
	if err != nil {
		return nil, fmt.Errorf("pulling %s failed: %s", parsedRef.String(), err)
	}

	var configDescriptor *ocispec.Descriptor

	for i := len(layers) - 1; i >= 0; i-- {
		switch descriptor := layers[i]; descriptor.MediaType {
		case helmTypes.ConfigMediaType:
			configDescriptor = &descriptor
			// remove the config descriptor from the list of descriptors
			layers = append(layers[:i], layers[i+1:]...)
		case ocispec.MediaTypeImageConfig:
			configDescriptor = &descriptor
			// remove the config descriptor from the list of descriptors
			layers = append(layers[:i], layers[i+1:]...)
		case distributionSchema.MediaTypeImageConfig:
			configDescriptor = &descriptor
			// remove the config descriptor from the list of descriptors
			layers = append(layers[:i], layers[i+1:]...)
		case distributionSchema.MediaTypeManifest:
			// see if we have the manifest in the descriptors list
			// remove the manifest descriptor from the list of descriptors
			layers = append(layers[:i], layers[i+1:]...)
		}
	}

	if configDescriptor == nil {
		return nil, fmt.Errorf("could not load config for artifact with manifest: %s", manifest.Digest)
	}

	result := &PullResult{
		Manifest: &descriptorPullSummary{
			MediaType: manifest.MediaType,
			Digest:    manifest.Digest.String(),
			Size:      manifest.Size,
		},
		Config: &descriptorPullSummary{
			MediaType: configDescriptor.MediaType,
			Digest:    configDescriptor.Digest.String(),
			Size:      configDescriptor.Size,
		},
		Layers: []*descriptorPullSummary{},
		Ref:    parsedRef.String(),
	}
	var getManifestErr error
	if _, manifestData, ok := memoryStore.Get(manifest); !ok {
		getManifestErr = errors.Errorf("Unable to retrieve blob with digest %s", manifest.Digest)
	} else {
		result.Manifest.Data = manifestData
	}
	if getManifestErr != nil {
		return nil, getManifestErr
	}
	var getConfigDescriptorErr error
	if _, configData, ok := memoryStore.Get(*configDescriptor); !ok {
		getConfigDescriptorErr = errors.Errorf("Unable to retrieve blob with digest %s", configDescriptor.Digest)
	} else {
		result.Config.Data = configData
	}
	if getConfigDescriptorErr != nil {
		return nil, getConfigDescriptorErr
	}

	var getLayerDescriptorErr error
	for _, layer := range layers {
		if _, layerData, ok := memoryStore.Get(layer); !ok {
			getLayerDescriptorErr = errors.Errorf("Unable to retrieve blob with digest %s", layer.Digest)
		} else {
			l := &descriptorPullSummary{
				MediaType: layer.MediaType,
				Digest:    layer.Digest.String(),
				Size:      layer.Size,
				Data:      layerData,
			}

			result.Layers = append(result.Layers, l)
		}
		if getLayerDescriptorErr != nil {
			return nil, getLayerDescriptorErr
		}
	}

	fmt.Fprintf(c.out, "Pulled: %s\n", result.Ref)
	fmt.Fprintf(c.out, "Digest: %s\n", result.Manifest.Digest)

	return result, nil
}

// PushResult is the result returned upon successful push.
type PushResult struct {
	Manifest *descriptorPushSummary   `json:"manifest"`
	Config   *descriptorPushSummary   `json:"config"`
	Layers   []*descriptorPushSummary `json:"layers"`
	Ref      string                   `json:"ref"`
}

type descriptorPushSummary struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// PushOption allows specifying various settings on push
type PushOption func(*pushOperation)

type pushOperation struct {
	strictMode bool
}

// Content is the content to be pushed
type Content struct {
	Data      []byte
	MediaType string
}

// Contents is a list of content to be pushed
type Contents []Content

// Push uploads a chart to a registry.
func (c *Client) Push(contents Contents, configMediaType string, ref string, options ...PushOption) (*PushResult, error) {
	parsedRef, err := registry.ParseReference(ref)
	if err != nil {
		return nil, err
	}

	operation := &pushOperation{
		strictMode: true, // By default, enable strict mode
	}

	for _, option := range options {
		option(operation)
	}

	descriptors, configDescriptor := []ocispec.Descriptor{}, ocispec.Descriptor{}
	memoryStore := content.NewMemory()
	for _, content := range contents {
		d, err := memoryStore.Add("", content.MediaType, content.Data)
		if err != nil {
			return nil, err
		}

		if d.MediaType == configMediaType {
			configDescriptor = d
			continue
		}

		descriptors = append(descriptors, d)
	}

	manifestData, manifest, err := content.GenerateManifest(&configDescriptor, nil, descriptors...)
	if err != nil {
		return nil, err
	}

	if err := memoryStore.StoreManifest(parsedRef.String(), manifest, manifestData); err != nil {
		return nil, err
	}

	registryStore := content.Registry{Resolver: c.resolver}
	_, err = oras.Copy(ctx(c.out, c.debug), memoryStore, parsedRef.String(), registryStore, "",
		oras.WithNameValidation(nil))
	if err != nil {
		return nil, err
	}

	layersSummary := []*descriptorPushSummary{}
	for _, layer := range descriptors {
		layersSummary = append(layersSummary, &descriptorPushSummary{
			Digest: layer.Digest.String(),
			Size:   layer.Size,
		})
	}

	result := &PushResult{
		Manifest: &descriptorPushSummary{
			Digest: manifest.Digest.String(),
			Size:   manifest.Size,
		},
		Config: &descriptorPushSummary{
			Digest: configDescriptor.Digest.String(),
			Size:   configDescriptor.Size,
		},
		Layers: layersSummary,
		Ref:    parsedRef.String(),
	}

	fmt.Fprintf(c.out, "Pushed: %s\n", result.Ref)
	fmt.Fprintf(c.out, "Digest: %s\n", result.Manifest.Digest)

	return result, err
}

// PushOptStrictMode returns a function that sets the strictMode setting on push
func PushOptStrictMode(strictMode bool) PushOption {
	return func(operation *pushOperation) {
		operation.strictMode = strictMode
	}
}

// Tags provides a sorted list all semver compliant tags for a given repository
func (c *Client) Tags(ref string) ([]string, error) {
	parsedReference, err := registry.ParseReference(ref)
	if err != nil {
		return nil, err
	}

	repository := registryremote.Repository{
		Reference: parsedReference,
		Client:    c.registryAuthorizer,
	}

	var registryTags []string

	for {
		registryTags, err = registry.Tags(ctx(c.out, c.debug), &repository)
		if err != nil {
			// Fallback to http based request
			if !repository.PlainHTTP && strings.Contains(err.Error(), "server gave HTTP response") {
				repository.PlainHTTP = true
				continue
			}
			return nil, err
		}

		break

	}

	var tagVersions []*semver.Version
	for _, tag := range registryTags {
		tagVersion, err := semver.StrictNewVersion(tag)
		if err == nil {
			tagVersions = append(tagVersions, tagVersion)
		}
	}

	// Sort the collection
	sort.Sort(sort.Reverse(semver.Collection(tagVersions)))

	tags := make([]string, len(tagVersions))

	for iTv, tv := range tagVersions {
		tags[iTv] = tv.String()
	}

	return tags, nil

}
