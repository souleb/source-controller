package registry

import (
	"fmt"
	"testing"

	"github.com/fluxcd/source-controller/pkg/registry"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
)

type mockRegistryClient struct {
	tags          []string
	LastCalledURL string
}

func (m *mockRegistryClient) Tags(url string) ([]string, error) {
	m.LastCalledURL = url
	return m.tags, nil
}

func (m *mockRegistryClient) Login(url string, opts ...registry.LoginOption) error {
	m.LastCalledURL = url
	return nil
}

func (m *mockRegistryClient) Logout(url string, opts ...registry.LogoutOption) error {
	m.LastCalledURL = url
	return nil
}

func (m *mockRegistryClient) Pull(ref string) (*registry.PullResult, error) {
	m.LastCalledURL = ref
	res := &registry.PullResult{
		Layers: []*registry.DescriptoPullSummary{
			{
				Digest: "sha256:1234567890123456789012345678989763456789012345678901234567890123",
				Size:   123456789,
				Data:   []byte("some data"),
			},
		},
	}

	return res, nil
}

func TestNewChartRegistry(t *testing.T) {
	client := &mockRegistryClient{}
	url := "localhost:5000/my_repo"
	t.Run("should construct chart registry", func(t *testing.T) {
		g := NewWithT(t)
		r, err := NewChartRegistry(url, client, registry.LoginOptBasicAuth("username", "password"))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(r).ToNot(BeNil())
		g.Expect(r.Host).To(Equal("localhost:5000"))
	})

	t.Run("should return error on invalid url", func(t *testing.T) {
		g := NewWithT(t)
		r, err := NewChartRegistry("localhost:5000 /my_repo", client, registry.LoginOptBasicAuth("username", "password"))
		g.Expect(err).To(HaveOccurred())
		g.Expect(r).To(BeNil())
	})
}

func TestGet(t *testing.T) {
	client := &mockRegistryClient{
		tags: []string{
			"0.0.1",
			"0.1.0",
			"0.1.1",
			"0.1.5+b.min.minute",
			"0.1.5+a.min.hour",
			"0.1.5+c.now",
			"0.2.0",
			"1.0.0",
			"1.1.0-rc.1",
		},
	}

	testCases := []struct {
		name        string
		version     string
		expected    string
		expectedErr string
	}{
		{
			name:     "should return latest stable version",
			version:  "",
			expected: "1.0.0",
		},
		{
			name:     "should return latest stable version (asterisk)",
			version:  "*",
			expected: "1.0.0",
		},
		{
			name:     "should return latest stable version (semver range)",
			version:  ">=0.1.5",
			expected: "1.0.0",
		},
		{
			name:     "should return 0.2.0 (semver range)",
			version:  "0.2.x",
			expected: "0.2.0",
		},
		{
			name:     "should return a perfect match",
			version:  "0.1.0",
			expected: "0.1.0",
		},
		{
			name:        "should an error for unfunfilled range",
			version:     ">2.0.0",
			expectedErr: "no candidates found for >2.0.0",
		},
	}

	url := "localhost:5000/my_repo"
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			r, err := NewChartRegistry(url, client, registry.LoginOptBasicAuth("username", "password"))
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(r).ToNot(BeNil())

			chart := "podinfo"
			cv, err := r.Get(chart, tc.version)
			if tc.expectedErr != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(Equal(tc.expectedErr))
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cv.URLs[0]).To(Equal(fmt.Sprintf("%s/%s:%s", url, chart, tc.expected)))
			g.Expect(client.LastCalledURL).To(Equal(fmt.Sprintf("%s/%s", url, chart)))
		})
	}
}

func TestDownloadChart(t *testing.T) {
	client := &mockRegistryClient{}
	testCases := []struct {
		name         string
		url          string
		chartVersion *repo.ChartVersion
		expected     string
		expectedErr  bool
	}{
		{
			name: "should download chart",
			url:  "localhost:5000/my_repo",
			chartVersion: &repo.ChartVersion{
				Metadata: &chart.Metadata{Name: "chart"},
				URLs:     []string{"localhost:5000/my_repo/podinfo:1.0.0"},
			},
			expected: "localhost:5000/my_repo/podinfo:1.0.0",
		},
		{
			name:         "no chart URL",
			chartVersion: &repo.ChartVersion{Metadata: &chart.Metadata{Name: "chart"}},
			expectedErr:  true,
		},
		{
			name: "invalid chart URL",
			chartVersion: &repo.ChartVersion{
				Metadata: &chart.Metadata{Name: "chart"},
				URLs:     []string{"localhost:5000 /my_repo/podinfo:1.0.0"},
			},
			expectedErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			r, err := NewChartRegistry(tc.url, client, registry.LoginOptBasicAuth("username", "password"))
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(r).ToNot(BeNil())
			res, err := r.DownloadChart(tc.chartVersion)
			if tc.expectedErr {
				g.Expect(err).To(HaveOccurred())
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(client.LastCalledURL).To(Equal(tc.expected))
			g.Expect(res).ToNot(BeNil())
			g.Expect(err).ToNot(HaveOccurred())
		})
	}

}
