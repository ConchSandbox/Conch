/*
Copyright the e2b-dev Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

[MODIFIED] - Changes made on 2026-08-05 by Team conch: Isolate network namespace lifecycle helpers.
*/
package netstack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func createNetworkNamespace(slot *Slot) (retErr error) {
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	netnsPath := slot.NetNSPath()
	target, err := os.OpenFile(netnsPath, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		return fmt.Errorf("reserve network namespace path %s: %w", netnsPath, err)
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(netnsPath)
		return fmt.Errorf("close network namespace path %s: %w", netnsPath, err)
	}

	runtime.LockOSThread()
	hostNS, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		_ = os.Remove(netnsPath)
		return fmt.Errorf("cannot get current (host) namespace: %w", err)
	}
	newNS := netns.None()
	mounted := false
	defer func() {
		if err := netns.Set(hostNS); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error resetting network namespace back to the host namespace: %w", err))
		}
		if newNS.IsOpen() {
			if err := newNS.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("error closing new network namespace: %w", err))
			}
		}
		if err := hostNS.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error closing host network namespace: %w", err))
		}
		runtime.UnlockOSThread()
		if retErr != nil {
			if mounted {
				_ = unix.Unmount(netnsPath, unix.MNT_DETACH)
			}
			_ = os.Remove(netnsPath)
		}
	}()

	newNS, err = netns.New()
	if err != nil {
		return fmt.Errorf("cannot create new namespace: %w", err)
	}
	nsPath := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
	if err := unix.Mount(nsPath, netnsPath, "bind", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mount network namespace at %s: %w", netnsPath, err)
	}
	mounted = true

	return nil
}

func isNetworkNamespaceMounted(netnsPath string) bool {
	var stat unix.Statfs_t
	if err := unix.Statfs(netnsPath, &stat); err != nil {
		return false
	}
	if stat.Type != unix.NSFS_MAGIC && stat.Type != unix.PROC_SUPER_MAGIC {
		return false
	}
	return true
}

func deleteNetworkNamespace(slot *Slot) error {
	if slot == nil {
		return nil
	}
	netnsPath := slot.NetNSPath()
	if _, err := os.Stat(netnsPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("error checking namespace %s: %w", netnsPath, err)
	}
	if err := unix.Unmount(netnsPath, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("error deleting namespace %s: %w", netnsPath, err)
	}
	if err := os.Remove(netnsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove namespace path %s: %w", netnsPath, err)
	}
	return nil
}

func runInNetNSPath(ctx context.Context, netnsPath string, fn func() error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("cannot get current namespace: %w", err)
	}
	defer func() {
		if err := netns.Set(hostNS); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error resetting network namespace back to host: %w", err))
		}
		if err := hostNS.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error closing host network namespace: %w", err))
		}
	}()

	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("cannot open network namespace %s: %w", netnsPath, err)
	}
	defer func() {
		if err := targetNS.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error closing target network namespace %s: %w", netnsPath, err))
		}
	}()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("error setting network namespace to %s: %w", netnsPath, err)
	}
	return fn()
}

// FlushSandboxConntrack removes every tracked connection from a slot's
// network namespace so connection state cannot cross sandbox assignments.
func FlushSandboxConntrack(ctx context.Context, slot *Slot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil {
		return nil
	}

	return runInNetNSPath(ctx, slot.NetNSPath(), func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Create the handle after entering the target namespace so its netlink
		// socket cannot bind to the host network namespace.
		handle, err := netlink.NewHandle(unix.NETLINK_NETFILTER)
		if err != nil {
			return fmt.Errorf("create conntrack netlink handle: %w", err)
		}
		defer handle.Close()

		if err := handle.ConntrackTableFlush(netlink.ConntrackTable); err != nil {
			return fmt.Errorf("flush conntrack table: %w", err)
		}
		return nil
	})
}
