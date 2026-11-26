package awsspot

import (
	"context"
	"errors"
	"fmt"
	"sluicework/internal/githubapp"
	"sluicework/internal/spot"
	"sync"
	"time"
)

type GitHubRunners struct {
	Client  *githubapp.Client
	mu      sync.Mutex
	pkg     spot.Package
	expires time.Time
}

func (g *GitHubRunners) runnerPackage(ctx context.Context) (spot.Package, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.expires) {
		return g.pkg, nil
	}
	p, e := g.Client.ChecksummedARMRunner(ctx)
	if e != nil {
		return spot.Package{}, errors.New("cannot verify the official ARM64 runner package")
	}
	g.pkg = spot.Package{URL: p.DownloadURL, SHA256: p.SHA256}
	g.expires = time.Now().Add(15 * time.Minute)
	return g.pkg, nil
}
func (g *GitHubRunners) Create(ctx context.Context, name, label string) (spot.Registration, error) {
	pkg, e := g.runnerPackage(ctx)
	if e != nil {
		return spot.Registration{}, fmt.Errorf("%w: package verification failed", spot.ErrRegistrationRejected)
	}
	grant, e := g.Client.Create(ctx, name, []string{"Linux", "ARM64", label})
	if e != nil {
		return spot.Registration{}, e
	}
	return spot.Registration{ID: grant.Runner.ID, JIT: grant.JIT, Package: pkg}, nil
}
func runner(r githubapp.Runner) spot.Runner {
	labels := []string{}
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	return spot.Runner{ID: r.ID, Online: r.Status == "online", Busy: r.Busy, Labels: labels}
}
func (g *GitHubRunners) Find(ctx context.Context, name string) (spot.Runner, bool, error) {
	all, e := g.Client.SharedGroupRunners(ctx)
	if e != nil {
		return spot.Runner{}, false, e
	}
	var found spot.Runner
	count := 0
	for _, r := range all {
		if r.Name == name {
			found = runner(r)
			count++
		}
	}
	if count > 1 {
		return found, false, errors.New("Spot registration name is ambiguous")
	}
	return found, count == 1, nil
}
func (g *GitHubRunners) Get(ctx context.Context, id int64) (spot.Runner, bool, error) {
	r, e := g.Client.Get(ctx, id)
	if errors.Is(e, githubapp.HTTPError(404)) {
		return spot.Runner{}, false, nil
	}
	if e != nil {
		return spot.Runner{}, false, e
	}
	return runner(r), true, nil
}
func (g *GitHubRunners) Remove(ctx context.Context, id int64) error {
	r, exists, e := g.Get(ctx, id)
	if e != nil {
		return e
	}
	if !exists {
		return nil
	}
	if r.Busy {
		return errors.New("Spot runner still reports an assigned job")
	}
	e = g.Client.Delete(ctx, id)
	if errors.Is(e, githubapp.HTTPError(404)) {
		return nil
	}
	return e
}

var _ spot.Registrations = (*GitHubRunners)(nil)
