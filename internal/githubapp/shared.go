package githubapp

import (
	"context"
	"errors"
	"fmt"
	"sluicework/internal/state"
	"sort"
	"strings"
)

func (c Config) validateSharing() error {
	if c.WorkstationGroupID == 0 && len(c.WorkstationRepositories) == 0 {
		return nil
	}
	if c.WorkstationGroupID <= 0 || !c.OrganizationRunners || len(c.WorkstationRepositories) == 0 || len(c.WorkstationRepositories) > 10 {
		return errors.New("shared workstations require organization runner management, a restricted group and 1–10 repositories")
	}
	owner := strings.Split(c.Repository, "/")[0]
	seen := map[string]bool{}
	for _, repo := range c.WorkstationRepositories {
		key := strings.ToLower(repo)
		if !repositoryPattern.MatchString(repo) || strings.Contains(repo, "..") || !strings.EqualFold(strings.Split(repo, "/")[0], owner) || seen[key] {
			return errors.New("shared workstation repositories must be distinct repositories in the configured organization")
		}
		seen[key] = true
	}
	if !seen[strings.ToLower(c.Repository)] {
		return errors.New("shared workstation repositories must include the primary repository")
	}
	return nil
}
func (c *Client) managedRunner(ctx context.Context, method, path string, body, out any) error {
	if c.config.WorkstationGroupID > 0 {
		return c.scopedRunner(ctx, "organization", method, path, body, out)
	}
	return c.repo(ctx, method, path, body, out)
}
func (c *Client) checkWorkstationGroup(ctx context.Context) error {
	token, err := c.managementToken(ctx, "organization_self_hosted_runners")
	if err != nil {
		return err
	}
	base := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d", strings.Split(c.config.Repository, "/")[0], c.config.WorkstationGroupID)
	var group struct {
		ID         int64  `json:"id"`
		Visibility string `json:"visibility"`
		Public     bool   `json:"allows_public_repositories"`
		Inherited  bool   `json:"inherited"`
		Restricted bool   `json:"restricted_to_workflows"`
	}
	if err = c.request(ctx, "GET", base, token, nil, &group); err != nil {
		return err
	}
	if group.ID != c.config.WorkstationGroupID || group.Visibility != "selected" || group.Public || group.Inherited || group.Restricted {
		return errors.New("use an organization-owned group with selected private repositories and no workflow restrictions")
	}
	var repos struct {
		Total        int `json:"total_count"`
		Repositories []struct {
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
		} `json:"repositories"`
	}
	if err = c.request(ctx, "GET", base+"/repositories?per_page=100", token, nil, &repos); err != nil {
		return err
	}
	if repos.Total != len(c.config.WorkstationRepositories) || len(repos.Repositories) != repos.Total {
		return errors.New("workstation group repository access differs from the approved repository list")
	}
	approved := map[string]bool{}
	for _, repo := range c.config.WorkstationRepositories {
		approved[strings.ToLower(repo)] = true
	}
	for _, repo := range repos.Repositories {
		if !repo.Private || !approved[strings.ToLower(repo.FullName)] {
			return errors.New("workstation group repository access differs from the approved repository list")
		}
	}
	return nil
}
func (b *Broker) machineScope() string {
	if b.primaryRepository != "" {
		return b.primaryRepository
	}
	return b.repository
}
func (b *Broker) PrimaryRepository() string { return b.machineScope() }
func (b *Broker) Repositories() []string {
	out := []string{b.repository}
	for _, child := range b.children {
		out = append(out, child.repository)
	}
	sort.Strings(out)
	return out
}
func (b *Broker) ForRepository(repository string) (*Broker, error) {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	if b.retired {
		return nil, state.ErrConflict
	}
	if repository == "" || strings.EqualFold(repository, b.repository) {
		return b, nil
	}
	if child := b.children[strings.ToLower(repository)]; child != nil {
		return child, nil
	}
	return nil, fmt.Errorf("%w: repository is not approved for this connection", state.ErrInvalid)
}
func (b *Broker) WorkstationGroups(ctx context.Context) ([]RunnerGroup, error) {
	source, ok := b.management.(interface {
		WorkstationGroups(context.Context) ([]RunnerGroup, error)
	})
	if !ok || !b.organization {
		return nil, errors.New("enable organization runner management to select a shared workstation group")
	}
	groups, err := source.WorkstationGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := []RunnerGroup{}
	for _, group := range groups {
		if group.Visibility == "selected" && !group.AllowsPublic && !group.Inherited && !group.Restricted {
			out = append(out, group)
		}
	}
	return out, nil
}
