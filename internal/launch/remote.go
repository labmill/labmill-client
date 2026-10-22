package launch

import (
	"context"
	"fmt"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
)

// loginRunner returns a runner on c's login node (its ssh_host) over the cached SSH client, logging in through the front
// end's dialog when needed. It backs app.Env.LoginRunner (remote_command).
func (cc *connCache) loginRunner(ctx context.Context, c config.Cluster) (backend.Runner, error) {
	if c.SSHHost == "" {
		return nil, fmt.Errorf("cluster %s has no login host configured", c.Name)
	}
	cl, err := cc.client(ctx, c.SSHHost)
	if err != nil {
		return nil, err
	}
	return &backend.SSHRunner{Client: cl}, nil
}
