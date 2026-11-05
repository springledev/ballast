package main

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/verify"
)

// drillSweepTimeout bounds the sweep of one Docker host, so one that
// cannot be reached is given up on rather than waited for.
const drillSweepTimeout = time.Minute

// startDrillSweep removes restore drill containers left behind by a
// Ballast process that could not clean up (killed, or the machine lost
// power), on every Docker host at once and in the background, so a slow
// or unreachable host never holds up the web server or a backup.
// Outcomes go to the log. The returned channel closes when it is done;
// wait for it before closing the app.
func (a *app) startDrillSweep(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		hosts, err := inventory.ListDockerHosts(a.store.DB())
		if err != nil {
			a.logf("checking for leftover test containers: %v", err)
			return
		}
		var wg sync.WaitGroup
		for _, host := range hosts {
			docker, err := a.ops.DockerHosts.Docker(host)
			if err != nil {
				a.logf("checking Docker host %s for leftover test containers: %v", host.Name, err)
				continue
			}
			// A machine without Docker (Proxmox or folders only) has
			// nothing to sweep and nothing worth a log line.
			if _, err := exec.LookPath(docker.Command(nil).Name); err != nil {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				hostCtx, cancel := context.WithTimeout(ctx, drillSweepTimeout)
				defer cancel()
				removed, err := verify.SweepDrills(hostCtx, a.ops.Runner, docker, a.ops.DrillOwner)
				for _, name := range removed {
					a.logf("removed test container %s and its temporary data from Docker host %s: a check was interrupted before it could clean up", name, host.Name)
				}
				if err != nil && ctx.Err() == nil {
					a.logf("checking for leftover test containers: %v", err)
				}
			}()
		}
		wg.Wait()
	}()
	return done
}
