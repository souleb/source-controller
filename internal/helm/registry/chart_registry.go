package registry

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/fluxcd/source-controller/pkg/registry"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
)

const OCIScheme string = "oci"

type RegistryClient interface {
	Login(host string, opts ...registry.LoginOption) error
	Logout(host string, opts ...registry.LogoutOption) error
	Tags(url string) ([]string, error)
	Pull(ref string) (*registry.PullResult, error)
}

type ChartRegistry struct {
	Host      string
	Namespace string
	// Options to configure the Client with while downloading the Index
	// or a chart from the URL.
	Options []registry.LoginOption

	RegistryClient RegistryClient
}

// NewChartRegistry constructs and returns a new ChartRegistry with
// the ChartRegistry.Client configured to the getter.Getter for the
// registry Host scheme. It returns an error on Host parsing failures,
// or if there is no getter available for the scheme.
func NewChartRegistry(registryURL string, client RegistryClient, opts ...registry.LoginOption) (*ChartRegistry, error) {
	if !strings.Contains(registryURL, OCIScheme) {
		registryURL = fmt.Sprintf("%s://%s", OCIScheme, registryURL)
	}

	registryURL = strings.TrimSuffix(registryURL, "/")

	u, err := url.Parse(registryURL)
	if err != nil {
		return nil, err
	}

	if client == nil {
		return nil, fmt.Errorf("registry client is required")
	}

	r := &ChartRegistry{
		Host:           u.Host,
		Namespace:      u.Path,
		RegistryClient: client,
		Options:        opts,
	}

	return r, nil
}

func (r *ChartRegistry) Login() error {
	err := r.RegistryClient.Login(r.Host, r.Options...)
	if err != nil {
		return err
	}
	return nil
}

func (r *ChartRegistry) Logout() error {
	err := r.RegistryClient.Logout(r.Host)
	if err != nil {
		return err
	}
	return nil
}

func (r *ChartRegistry) Get(name, version string) (*repo.ChartVersion, error) {
	if err := validateOCIURL(fmt.Sprint(r.Host, r.Namespace, "/", name)); err != nil {
		return nil, err
	}
	// get a sorted list of tags
	tags, err := r.RegistryClient.Tags(fmt.Sprint(r.Host, r.Namespace, "/", name))
	if err != nil {
		return nil, fmt.Errorf("failed to get tags for %s: %w", fmt.Sprint(r.Host, r.Namespace, "/", name), err)
	}

	c, err := semver.NewConstraint("*")
	if err != nil {
		return nil, err
	}
	latestStable := version == "" || version == "*"
	if !latestStable {
		c, err = semver.NewConstraint(version)
		if err != nil {
			return nil, err
		}
	}

	var candidates []*semver.Version
	for _, t := range tags {
		v, err := semver.NewVersion(t)
		if err != nil {
			continue
		}

		if c != nil && !c.Check(v) {
			continue
		}

		candidates = append(candidates, v)
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates found for %s", version)
	}

	sort.Sort(sort.Reverse(semver.Collection(candidates)))
	chart := &repo.ChartVersion{
		Metadata: &chart.Metadata{
			Name:    name,
			Version: candidates[0].Original(),
		},
		URLs: []string{fmt.Sprintf("%s:%s", fmt.Sprint(r.Host, r.Namespace, "/", name), candidates[0].Original())},
	}

	return chart, nil

}

func (r *ChartRegistry) DownloadChart(chart *repo.ChartVersion) (*bytes.Buffer, error) {
	if len(chart.URLs) == 0 {
		return nil, fmt.Errorf("chart '%s' has no downloadable URL", chart.Name)
	}

	if err := validateOCIURL(chart.URLs[0]); err != nil {
		return nil, err
	}

	res, err := r.RegistryClient.Pull(chart.URLs[0])
	if err != nil {
		return nil, err
	}

	return bytes.NewBuffer(res.Layers[0].Data), nil

}

func validateOCIURL(ociURL string) error {
	if !strings.Contains(ociURL, OCIScheme) {
		ociURL = fmt.Sprintf("%s://%s", OCIScheme, ociURL)
	}

	_, err := url.Parse(ociURL)
	if err != nil {
		return fmt.Errorf("invalid oci URL format '%s': %w", ociURL, err)
	}

	return nil
}
