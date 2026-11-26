package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type RunnerGroup struct {
	Visibility   string `json:"visibility"`
	AllowsPublic bool   `json:"allows_public_repositories"`
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Inherited    bool   `json:"inherited"`
	Restricted   bool   `json:"restricted_to_workflows"`
}
type OrganizationRunner struct {
	Runner
	Group     RunnerGroup
	Available bool
}

func (c *Client) scopedRunner(ctx context.Context, scope, method, path string, body, out any) error {
	if scope == "repository" {
		return c.repo(ctx, method, path, body, out)
	}
	if scope != "organization" {
		return errors.New("invalid runner scope")
	}
	token, err := c.managementToken(ctx, "organization_self_hosted_runners")
	if err != nil {
		return err
	}
	return c.request(ctx, method, "/orgs/"+strings.Split(c.config.Repository, "/")[0]+"/actions/runners"+path, token, body, out)
}
func (c *Client) groups(ctx context.Context, visible bool) ([]RunnerGroup, error) {
	token, err := c.managementToken(ctx, "organization_self_hosted_runners")
	if err != nil {
		return nil, err
	}
	result := []RunnerGroup{}
	for page := 1; page <= 10; page++ {
		path := fmt.Sprintf("/orgs/%s/actions/runner-groups?per_page=100&page=%d", strings.Split(c.config.Repository, "/")[0], page)
		if visible {
			path += "&visible_to_repository=" + url.QueryEscape(strings.Split(c.config.Repository, "/")[1])
		}
		var out struct {
			Total  int           `json:"total_count"`
			Groups []RunnerGroup `json:"runner_groups"`
		}
		if err = c.request(ctx, "GET", path, token, nil, &out); err != nil {
			return nil, err
		}
		result = append(result, out.Groups...)
		if out.Total > 1000 {
			return nil, errors.New("too many organization runner groups")
		}
		if len(out.Groups) < 100 || len(result) >= out.Total {
			return result, nil
		}
	}
	return nil, errors.New("runner group pagination limit reached")
}
func (c *Client) OrganizationInventory(ctx context.Context) ([]OrganizationRunner, error) {
	groups, err := c.groups(ctx, false)
	if err != nil {
		return nil, err
	}
	visible, err := c.groups(ctx, true)
	if err != nil {
		return nil, err
	}
	allowed := map[int64]bool{}
	for _, g := range visible {
		allowed[g.ID] = true
	}
	token, err := c.managementToken(ctx, "organization_self_hosted_runners")
	if err != nil {
		return nil, err
	}
	result := []OrganizationRunner{}
	for _, g := range groups {
		for page := 1; page <= 20; page++ {
			var out struct {
				Total   int      `json:"total_count"`
				Runners []Runner `json:"runners"`
			}
			path := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/runners?per_page=100&page=%d", strings.Split(c.config.Repository, "/")[0], g.ID, page)
			if err = c.request(ctx, "GET", path, token, nil, &out); err != nil {
				return nil, err
			}
			if len(result)+len(out.Runners) > 2000 {
				return nil, errors.New("too many organization runners")
			}
			for _, r := range out.Runners {
				result = append(result, OrganizationRunner{r, g, allowed[g.ID]})
			}
			if len(out.Runners) < 100 || page*100 >= out.Total {
				break
			}
			if page == 20 {
				return nil, errors.New("runner group member pagination limit reached")
			}
		}
	}
	return result, nil
}

func (c *Client) WorkstationGroups(ctx context.Context) ([]RunnerGroup, error) {
	return c.groups(ctx, false)
}
