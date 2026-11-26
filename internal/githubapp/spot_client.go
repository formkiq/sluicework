package githubapp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SharedGroupRunners lists only the connection's explicitly restricted group.
// It never exposes registrations from unrelated organization groups.
func (c *Client) SharedGroupRunners(ctx context.Context) ([]Runner, error) {
	if c.config.WorkstationGroupID <= 0 {
		return nil, errors.New("configure a restricted private runner group")
	}
	if e := c.checkWorkstationGroup(ctx); e != nil {
		return nil, e
	}
	token, e := c.managementToken(ctx, "organization_self_hosted_runners")
	if e != nil {
		return nil, e
	}
	result := []Runner{}
	for page := 1; page <= 10; page++ {
		var out struct {
			Total   int      `json:"total_count"`
			Runners []Runner `json:"runners"`
		}
		path := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/runners?per_page=100&page=%d", strings.Split(c.config.Repository, "/")[0], c.config.WorkstationGroupID, page)
		if e = c.request(ctx, "GET", path, token, nil, &out); e != nil {
			return nil, e
		}
		if out.Total > 1000 {
			return nil, errors.New("runner group exceeds the supported size")
		}
		result = append(result, out.Runners...)
		if len(out.Runners) < 100 || page*100 >= out.Total {
			return result, nil
		}
	}
	return nil, errors.New("runner group pagination limit exceeded")
}

// ChecksummedARMRunner resolves the checksum from GitHub's official release
// asset metadata when the organization download API omits sha256_checksum.
// The public request never carries an installation token.
func (c *Client) ChecksummedARMRunner(ctx context.Context) (RunnerApplication, error) {
	apps, e := c.Applications(ctx, "organization")
	if e != nil {
		return RunnerApplication{}, e
	}
	re := regexp.MustCompile(`^https://github.com/actions/runner/releases/download/(v[0-9]+\.[0-9]+\.[0-9]+)/(actions-runner-linux-arm64-[0-9]+\.[0-9]+\.[0-9]+\.tar\.gz)$`)
	for _, p := range apps {
		if p.OS != "linux" || p.Architecture != "arm64" {
			continue
		}
		parts := re.FindStringSubmatch(p.DownloadURL)
		if len(parts) != 3 {
			continue
		}
		if regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.SHA256) {
			return p, nil
		}
		var release struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
			Assets     []struct {
				Name   string `json:"name"`
				URL    string `json:"browser_download_url"`
				Digest string `json:"digest"`
			} `json:"assets"`
		}
		if e = c.request(ctx, "GET", "/repos/actions/runner/releases/tags/"+parts[1], "", nil, &release); e != nil {
			return RunnerApplication{}, e
		}
		if release.Tag != parts[1] || release.Draft || release.Prerelease {
			return RunnerApplication{}, errors.New("invalid official runner release")
		}
		matches := 0
		for _, a := range release.Assets {
			if a.Name == parts[2] && a.URL == p.DownloadURL && regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(a.Digest) {
				matches++
				p.SHA256 = strings.TrimPrefix(a.Digest, "sha256:")
			}
		}
		if matches == 1 {
			return p, nil
		}
		return RunnerApplication{}, errors.New("official runner release did not provide one checksummed ARM64 asset")
	}
	return RunnerApplication{}, errors.New("no supported official ARM64 runner package")
}
