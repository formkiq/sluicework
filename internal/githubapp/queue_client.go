package githubapp

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// QueueRequest uses a separate, repository-scoped token. The queue never leases
// administration-write or runner-registration credentials to a build worker.
func (c *Client) QueueRequest(ctx context.Context, method, path string, body, out any) error {
	if !strings.HasPrefix(path, "/repos/"+c.config.Repository+"/") && !strings.HasPrefix(path, "/orgs/"+strings.Split(c.config.Repository, "/")[0]+"/") {
		return errors.New("queue request outside configured scope")
	}
	token, err := c.queueToken(ctx)
	if err != nil {
		return err
	}
	return c.request(ctx, method, path, token, body, out)
}
func (c *Client) queueToken(ctx context.Context) (string, error) {
	c.managementMu.Lock()
	defer c.managementMu.Unlock()
	if cached := c.managementTokens["priority-queue"]; time.Until(cached.Expires) > time.Minute {
		return cached.Value, nil
	}
	jwt, err := c.jwt(time.Now())
	if err != nil {
		return "", err
	}
	permissions := map[string]string{"actions": "write", "checks": "write", "pull_requests": "read", "contents": "read", "administration": "read", "metadata": "read", "actions_variables": "read"}
	if c.config.OrganizationRunners {
		permissions["organization_self_hosted_runners"] = "read"
	}
	var result RegistrationToken
	err = c.request(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", c.config.InstallationID), jwt, map[string]any{"repositories": []string{strings.Split(c.config.Repository, "/")[1]}, "permissions": permissions}, &result)
	if err != nil {
		return "", err
	}
	if result.Token == "" || time.Until(result.ExpiresAt) < time.Minute {
		return "", errors.New("invalid queue installation token")
	}
	if c.managementTokens == nil {
		c.managementTokens = map[string]cachedManagementToken{}
	}
	c.managementTokens["priority-queue"] = cachedManagementToken{result.Token, result.ExpiresAt}
	return result.Token, nil
}
func (c *Client) QueueRunners(ctx context.Context) ([]Runner, error) {
	byID := map[int64]Runner{}
	base := "/repos/" + c.config.Repository + "/actions/runners"
	for page := 1; page <= 20; page++ {
		var result struct {
			Total   int      `json:"total_count"`
			Runners []Runner `json:"runners"`
		}
		if err := c.QueueRequest(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", base, page), nil, &result); err != nil {
			return nil, err
		}
		if result.Total > 2000 {
			return nil, errors.New("too many runners")
		}
		for _, r := range result.Runners {
			byID[r.ID] = r
		}
		if len(result.Runners) < 100 || page*100 >= result.Total {
			break
		}
	}
	if c.config.OrganizationRunners {
		owner, repo, _ := strings.Cut(c.config.Repository, "/")
		// Only unrestricted groups visible to this repository can supply a target.
		var groups struct {
			Total  int           `json:"total_count"`
			Groups []RunnerGroup `json:"runner_groups"`
		}
		if err := c.QueueRequest(ctx, "GET", "/orgs/"+owner+"/actions/runner-groups?per_page=100&visible_to_repository="+url.QueryEscape(repo), nil, &groups); err != nil {
			return nil, err
		}
		if groups.Total > 100 {
			return nil, errors.New("too many visible runner groups")
		}
		for _, group := range groups.Groups {
			if group.Restricted || group.AllowsPublic {
				continue
			}
			for page := 1; page <= 20; page++ {
				var result struct {
					Total   int      `json:"total_count"`
					Runners []Runner `json:"runners"`
				}
				path := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/runners?per_page=100&page=%d", owner, group.ID, page)
				if err := c.QueueRequest(ctx, "GET", path, nil, &result); err != nil {
					return nil, err
				}
				if result.Total > 2000 {
					return nil, errors.New("too many group runners")
				}
				for _, r := range result.Runners {
					r.GroupName = group.Name
					byID[r.ID] = r
				}
				if len(result.Runners) < 100 || page*100 >= result.Total {
					break
				}
			}
		}
	}
	result := make([]Runner, 0, len(byID))
	for _, r := range byID {
		result = append(result, r)
	}
	return result, nil
}

// QueueArtifact downloads only a bounded one-file metadata archive. Redirect
// credentials are deliberately not forwarded to GitHub's signed storage URL.
func (c *Client) QueueArtifact(ctx context.Context, id int64, digest string) ([]byte, error) {
	if id <= 0 || !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return nil, errors.New("invalid request artifact digest")
	}
	token, err := c.queueToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.origin+fmt.Sprintf("/repos/%s/actions/artifacts/%d/zip", c.config.Repository, id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("request artifact download failed")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound {
		return nil, errors.New("expected a signed artifact download")
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil || location.Scheme != "https" || location.User != nil || location.Hostname() == "" {
		return nil, errors.New("invalid artifact download origin")
	}
	host := location.Hostname()
	if !strings.HasSuffix(host, ".blob.core.windows.net") && !strings.HasSuffix(host, ".actions.githubusercontent.com") && host != "objects.githubusercontent.com" {
		return nil, errors.New("unrecognized artifact download origin")
	}
	unsigned, err := http.NewRequestWithContext(ctx, "GET", location.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err = c.http.Do(unsigned)
	if err != nil {
		return nil, errors.New("request artifact storage download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("request artifact storage returned an error")
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, 32769))
	if err != nil || len(archive) > 32768 {
		return nil, errors.New("request artifact exceeds metadata limit")
	}
	hash := sha256.Sum256(archive)
	if "sha256:"+hex.EncodeToString(hash[:]) != digest {
		return nil, errors.New("request artifact digest mismatch")
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(zr.File) != 1 {
		return nil, errors.New("request artifact must contain one file")
	}
	file := zr.File[0]
	if file.Name != "sluicework-request.json" || file.UncompressedSize64 > 16384 || !file.Mode().IsRegular() {
		return nil, errors.New("invalid request metadata file")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errors.New("request metadata exceeds limit")
	}
	return data, nil
}
