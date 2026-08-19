// docker.go manages the one container Anamnesia needs: Postgres with
// pgvector.
//
// There is no compose file and no Anamnesia image. The binary you already
// downloaded is the server, so the only thing that genuinely has to be
// containerised is the database. Anamnesia drives the `docker` CLI rather
// than the Engine API: the CLI is present wherever Docker is, works
// unchanged against Docker Desktop, OrbStack, colima and podman's docker
// shim, and its errors are already written for humans.
//
// Anamnesia only ever touches a container whose name matches
// postgres.container, and a volume matching postgres.volume. It never
// stops, removes or prunes anything else.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// dockerBin is the client Anamnesia shells out to.
const dockerBin = "docker"

// errDockerMissing means Docker is not usable on this machine.
var errDockerMissing = errors.New("docker is not available")

// requireDocker checks that the CLI exists and the daemon answers.
func requireDocker(ctx context.Context) error {
	if _, err := exec.LookPath(dockerBin); err != nil {
		return fmt.Errorf("%w: the `docker` command is not on your PATH. Install Docker Desktop, OrbStack or colima, then run `anamnesia start` again", errDockerMissing)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, dockerBin, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: the Docker daemon is not responding. Start Docker and try again.\n%s",
			errDockerMissing, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerVersion returns the daemon version, or an error string.
func dockerVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dockerBin, "info", "--format", "{{.ServerVersion}}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// containerState is the lifecycle state of a named container.
type containerState string

const (
	stateMissing containerState = "missing"
	stateRunning containerState = "running"
	stateStopped containerState = "stopped"
)

// inspectContainer reports whether the container exists and is running.
// containerOwner reports the ANAMNESIA_HOME recorded on a container when it
// was created. Empty means the container predates ownership labels.
func containerOwner(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dockerBin, "inspect", "--format",
		"{{index .Config.Labels \""+containerOwnerLabel+"\"}}", name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %s", name, strings.TrimSpace(string(out)))
	}
	owner := strings.TrimSpace(string(out))
	// A missing label prints as "<no value>" with this format.
	if owner == "<no value>" {
		return "", nil
	}
	return owner, nil
}

func inspectContainer(ctx context.Context, name string) (containerState, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dockerBin,
		"inspect", "--format", "{{.State.Running}}", name).CombinedOutput()
	if err != nil {
		// `docker inspect` fails for an unknown name, which is not an
		// error condition for us.
		if strings.Contains(string(out), "No such object") ||
			strings.Contains(string(out), "no such object") {
			return stateMissing, nil
		}
		return stateMissing, fmt.Errorf("docker inspect %s: %s", name, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) == "true" {
		return stateRunning, nil
	}
	return stateStopped, nil
}

// ensurePostgres brings the database container up and waits until it
// accepts connections. Idempotent: safe to call when already running.
func ensurePostgres(ctx context.Context, hc *hostConfig, out io.Writer) error {
	if !hc.ManagesPostgres() {
		return nil // user supplied their own postgres.url
	}
	if err := requireDocker(ctx); err != nil {
		return err
	}
	name := hc.Get("postgres.container")
	state, err := inspectContainer(ctx, name)
	if err != nil {
		return err
	}
	ourHome, err := anamnesiaHome()
	if err != nil {
		return err
	}
	// An existing container may belong to another install. Decide before
	// touching it: the password reconcile below repairs a drifted config by
	// rewriting the role password, which against someone else's database
	// locks them out of their own memory.
	decision := containerDecision{mayUse: true, mayResetPassword: true}
	if state != stateMissing {
		owner, err := containerOwner(ctx, name)
		if err != nil {
			return err
		}
		if decision, err = decideContainer(owner, ourHome, adoptContainer); err != nil {
			return err
		}
	}
	switch state {
	case stateRunning:
		// Still wait for readiness: "running" only means the process
		// started, not that Postgres finished recovery.
	case stateStopped:
		fmt.Fprintf(out, "  starting container %s\n", name)
		if o, err := exec.CommandContext(ctx, dockerBin, "start", name).CombinedOutput(); err != nil {
			return fmt.Errorf("docker start %s: %s", name, strings.TrimSpace(string(o)))
		}
	case stateMissing:
		if err := createPostgres(ctx, hc, out); err != nil {
			return err
		}
	}
	if err := waitPostgresReady(ctx, hc, out); err != nil {
		return err
	}
	return reconcilePostgresPassword(ctx, hc, out, decision)
}

// reconcilePostgresPassword makes the running database accept the password in
// the config.
//
// POSTGRES_PASSWORD only takes effect when the data directory is first
// initialised. So a data volume that outlives its config keeps the old
// password, and the pairing happens more often than it sounds: reinstalling
// while keeping your memory, restoring ~/.anamnesia from a backup, or setting
// postgres.password by hand all produce it. Left alone it surfaces as a bare
// "password authentication failed", which says nothing about the cause or the
// fix.
//
// initdb trusts connections over the local unix socket, so the password can be
// set from inside the container without knowing the old one.
func reconcilePostgresPassword(ctx context.Context, hc *hostConfig, out io.Writer, decision containerDecision) error {
	// Give a just-started server a moment before concluding anything. An
	// authenticated connection can still be refused for a beat after the port
	// opens, and treating that as a wrong password would rewrite the password
	// on every first run and say so, which is both needless and misleading.
	if waitPasswordWorks(ctx, hc, 15*time.Second) {
		return nil
	}
	if !decision.mayResetPassword {
		home, _ := anamnesiaHome()
		return passwordResetRefused(hc.Get("postgres.container"), home)
	}
	fmt.Fprintln(out, "  the stored database password differs from your config; updating it")
	if err := setPostgresPassword(ctx, hc); err != nil {
		return passwordMismatchError(hc, err)
	}
	if !waitPasswordWorks(ctx, hc, 10*time.Second) {
		return passwordMismatchError(hc, nil)
	}
	return nil
}

// waitPasswordWorks polls until an authenticated connection succeeds or the
// budget runs out.
func waitPasswordWorks(ctx context.Context, hc *hostConfig, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		if postgresPasswordWorks(ctx, hc) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// postgresPasswordWorks connects the way the server does: from the host, over
// the published port, with the configured password.
//
// Testing from inside the container instead would always pass. initdb's
// pg_hba grants `trust` to loopback, and pg_hba is first-match, so an
// in-container connection to 127.0.0.1 never presents a password at all;
// only connections arriving from outside reach the password-checking rule.
func postgresPasswordWorks(ctx context.Context, hc *hostConfig) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, hc.DatabaseURL())
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return conn.Ping(ctx) == nil
}

// setPostgresPassword sets the role's password over the trusted local socket.
func setPostgresPassword(ctx context.Context, hc *hostConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	user := hc.Get("postgres.user")
	// The password travels as a psql variable, so whatever it contains cannot
	// terminate the SQL string. The statement arrives on stdin rather than via
	// -c because -c sends its argument straight to the server, without the
	// variable interpolation that :'pw' needs.
	cmd := exec.CommandContext(ctx, dockerBin, "exec", "-i",
		hc.Get("postgres.container"),
		"psql", "-U", user, "-d", hc.Get("postgres.database"),
		"--set=pw="+hc.Get("postgres.password"),
		"-v", "ON_ERROR_STOP=1", "-tA", "-f", "-",
	)
	cmd.Stdin = strings.NewReader(
		fmt.Sprintf("ALTER USER %s WITH PASSWORD :'pw';\n", quoteIdent(user)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// quoteIdent quotes a SQL identifier.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// passwordMismatchError explains the situation and the ways out, since none of
// them is guessable from a Postgres auth error.
func passwordMismatchError(hc *hostConfig, cause error) error {
	msg := fmt.Sprintf(`the database in volume %q does not accept the password in %s

That volume was initialised with a different password, and Postgres keeps the
original: POSTGRES_PASSWORD only applies the first time a data directory is
created. Pick one:

  - put the old password back:  anamnesia config set postgres.password <old>
  - start over, losing every stored memory:
      docker rm -f %s && docker volume rm %s && anamnesia start`,
		hc.Get("postgres.volume"), hc.GlobalPath,
		hc.Get("postgres.container"), hc.Get("postgres.volume"))
	if cause != nil {
		return fmt.Errorf("%s\n\nsetting it automatically failed: %w", msg, cause)
	}
	return errors.New(msg)
}

// createPostgres pulls the image if needed and creates the container with
// a named volume, bound to loopback only.
func createPostgres(ctx context.Context, hc *hostConfig, out io.Writer) error {
	image := hc.Get("postgres.image")
	name := hc.Get("postgres.container")
	volume := hc.Get("postgres.volume")
	port := hc.Int("postgres.port")

	if hc.Get("postgres.password") == "" {
		return errors.New("postgres.password is empty; run `anamnesia setup` to generate one")
	}

	fmt.Fprintf(out, "  pulling %s (first run only, this can take a minute)\n", image)
	pull := exec.CommandContext(ctx, dockerBin, "pull", image)
	pull.Stdout = io.Discard
	pull.Stderr = io.Discard
	if err := pull.Run(); err != nil {
		// Not fatal on its own: the image may already be present locally
		// while the registry is unreachable. `docker run` decides.
		fmt.Fprintf(out, "  could not pull %s, trying the local copy\n", image)
	}

	ownerHome, err := anamnesiaHome()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  creating container %s on 127.0.0.1:%d\n", name, port)
	args := []string{
		"run", "--detach",
		"--name", name,
		"--restart", "unless-stopped",
		"--publish", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"--volume", volume + ":/var/lib/postgresql/data",
		"--env", "POSTGRES_USER=" + hc.Get("postgres.user"),
		"--env", "POSTGRES_PASSWORD=" + hc.Get("postgres.password"),
		"--env", "POSTGRES_DB=" + hc.Get("postgres.database"),
		"--label", "anamnesia=postgres",
		// Records which install owns this container, so a second
		// ANAMNESIA_HOME that resolves to the same name is refused rather
		// than silently sharing the database and rewriting its password.
		"--label", containerOwnerLabel + "=" + ownerHome,
		image,
	}
	if o, err := exec.CommandContext(ctx, dockerBin, args...).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(o))
		if strings.Contains(msg, "port is already allocated") ||
			strings.Contains(msg, "address already in use") {
			return fmt.Errorf("port %d is already in use. Pick another with `anamnesia config set postgres.port <n>` and run `anamnesia start` again", port)
		}
		return fmt.Errorf("docker run %s: %s", image, msg)
	}
	return nil
}

// waitPostgresReady waits until the container's real Postgres accepts TCP
// connections.
//
// Two details here are not incidental, and removing either brings back a
// first-run failure that looks like a corrupt install.
//
// The check is over TCP (-h 127.0.0.1), not the default unix socket. On the
// very first start the postgres image runs a *temporary* server to create the
// database, then shuts it down and starts the real one. That temporary server
// listens on the socket only, so a socket-based pg_isready reports success
// during initialisation; connecting then gets the handshake cut off with
// "unexpected EOF" the moment the temporary server exits. The temporary
// server never listens on TCP, which is what makes this the discriminator.
//
// And success has to repeat before we believe it, so that a probe landing in
// the gap between the two servers cannot pass.
func waitPostgresReady(ctx context.Context, hc *hostConfig, out io.Writer) error {
	name := hc.Get("postgres.container")
	user := hc.Get("postgres.user")
	db := hc.Get("postgres.database")

	const wantConsecutive = 2
	deadline := time.Now().Add(120 * time.Second)
	announced := false
	streak := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, dockerBin, "exec", name,
			"pg_isready", "-h", "127.0.0.1", "-p", "5432", "-U", user, "-d", db, "-q")
		if err := cmd.Run(); err == nil {
			streak++
			if streak >= wantConsecutive {
				return nil
			}
		} else {
			streak = 0
			// Only say we are waiting once something has actually refused a
			// connection, so an already-running database starts silently.
			if !announced {
				fmt.Fprintln(out, "  waiting for postgres to accept connections")
				announced = true
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres in container %s did not become ready within 120s; check `docker logs %s`", name, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// stopPostgres stops the container but keeps it and its volume.
func stopPostgres(ctx context.Context, hc *hostConfig, out io.Writer) error {
	if !hc.ManagesPostgres() {
		return nil
	}
	name := hc.Get("postgres.container")
	state, err := inspectContainer(ctx, name)
	if err != nil || state != stateRunning {
		return nil
	}
	fmt.Fprintf(out, "  stopping container %s\n", name)
	if o, err := exec.CommandContext(ctx, dockerBin, "stop", name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop %s: %s", name, strings.TrimSpace(string(o)))
	}
	return nil
}

// removePostgres deletes the container, and the volume when data is true.
// Only ever called from `anamnesia uninstall --purge`.
func removePostgres(ctx context.Context, hc *hostConfig, out io.Writer, data bool) error {
	if !hc.ManagesPostgres() {
		return nil
	}
	name := hc.Get("postgres.container")
	fmt.Fprintf(out, "  removing container %s\n", name)
	_ = exec.CommandContext(ctx, dockerBin, "rm", "--force", name).Run()
	if data {
		volume := hc.Get("postgres.volume")
		fmt.Fprintf(out, "  removing volume %s (all stored memory)\n", volume)
		if o, err := exec.CommandContext(ctx, dockerBin, "volume", "rm", volume).CombinedOutput(); err != nil {
			return fmt.Errorf("docker volume rm %s: %s", volume, strings.TrimSpace(string(o)))
		}
	}
	return nil
}

// pullPostgresImage refreshes the image, used by `anamnesia update`.
func pullPostgresImage(ctx context.Context, hc *hostConfig, out io.Writer) error {
	if !hc.ManagesPostgres() {
		return nil
	}
	if err := requireDocker(ctx); err != nil {
		return err
	}
	image := hc.Get("postgres.image")
	fmt.Fprintf(out, "  pulling %s\n", image)
	cmd := exec.CommandContext(ctx, dockerBin, "pull", image)
	cmd.Stdout = io.Discard
	cmd.Stderr = out
	return cmd.Run()
}
