package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/pkg/ulog"
)

// Exempt from authorization: no body, no sandbox state, and probes routinely
// run under another account.
const healthPath = "/health"

var errPeerNotAuthorized = errors.New("peer is not authorized to use the conch control plane")

// peerIdentity is what SO_PEERCRED reported at connect(2).
type peerIdentity struct {
	PID int32
	UID uint32
	GID uint32
}

func (p peerIdentity) String() string {
	return fmt.Sprintf("uid=%d gid=%d pid=%d", p.UID, p.GID, p.PID)
}

type peerIdentityKey struct{}

// peerCredential reads SO_PEERCRED, which the kernel fixes at connect(2) and
// the peer cannot change afterwards. PID is for audit only: it may have been
// reused by the time the request is handled.
func peerCredential(conn *net.UnixConn) (peerIdentity, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return peerIdentity{}, fmt.Errorf("access unix connection: %w", err)
	}
	var (
		cred    *unix.Ucred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return peerIdentity{}, fmt.Errorf("read peer credentials: %w", err)
	}
	if credErr != nil {
		return peerIdentity{}, fmt.Errorf("read peer credentials: %w", credErr)
	}
	return peerIdentity{PID: cred.Pid, UID: cred.Uid, GID: cred.Gid}, nil
}

// peerIdentityConnContext runs as http.Server.ConnContext because that is the
// only hook with both the conn and the request context: on a Unix socket
// Request.RemoteAddr is the empty "@" address. No identity means denial.
func peerIdentityConnContext(ctx context.Context, conn net.Conn) context.Context {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return ctx
	}
	identity, err := peerCredential(unixConn)
	if err != nil {
		ulog.GetLogger().Error("Failed to read control-plane peer credentials", ulog.F("error", err))
		return ctx
	}
	return context.WithValue(ctx, peerIdentityKey{}, identity)
}

func peerIdentityFrom(ctx context.Context) (peerIdentity, bool) {
	identity, ok := ctx.Value(peerIdentityKey{}).(peerIdentity)
	return identity, ok
}

// authorizePeer admits root and nothing else. conchd runs as root, so the
// socket is root-owned 0660 and root is already the only account that can open
// it; this repeats that decision inside the daemon, where it is auditable and
// where it still holds if the socket mode is later widened by hand.
func authorizePeer(identity peerIdentity) error {
	if identity.UID == 0 {
		return nil
	}
	return errPeerNotAuthorized
}

// peerAuthMiddleware fails closed: a request without peer credentials is
// denied, so a transport that does not populate them cannot silently open the
// control plane.
func peerAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthPath {
			next.ServeHTTP(w, r)
			return
		}
		logger := ulog.GetLogger()
		identity, ok := peerIdentityFrom(r.Context())
		if !ok {
			logger.Warn("Rejected control-plane request without peer credentials",
				ulog.F("method", r.Method),
				ulog.F("path", r.URL.Path),
			)
			http.Error(w, "Forbidden: caller identity unavailable", http.StatusForbidden)
			return
		}
		if err := authorizePeer(identity); err != nil {
			logger.Warn("Rejected unauthorized control-plane request",
				ulog.F("method", r.Method),
				ulog.F("path", r.URL.Path),
				ulog.F("peer", identity.String()),
				ulog.F("error", err),
			)
			http.Error(w, "Forbidden: the conch control plane is restricted to root", http.StatusForbidden)
			return
		}
		logger.Debug("Authorized control-plane request",
			ulog.F("method", r.Method),
			ulog.F("path", r.URL.Path),
			ulog.F("peer", identity.String()),
		)
		next.ServeHTTP(w, r)
	})
}
