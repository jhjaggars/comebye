package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jhjaggars/comebye/internal/agenttoken"
	"github.com/jhjaggars/comebye/internal/apipy"
)

// exitStatus ends a command with this status and no message of its own
// (the script `comebye api run` ran already said why).
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// cmdAPI is `comebye api`: a read-only Python API over the hub for scripts.
func cmdAPI(e *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(e.Stdout, apipy.Help)
		return nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(e.Stdout, apipy.Help)
		return nil
	case "types":
		return apiTypes(e, args[1:])
	case "run":
		if len(args) < 2 {
			return usageErr("run needs a script: FILE, or - for standard input")
		}
		if args[1] == "-h" || args[1] == "--help" {
			fmt.Fprint(e.Stdout, apipy.Help)
			return nil
		}
		return apiRun(e, args[1], args[2:])
	}
	return usageErr("unknown subcommand %q (run, types or --help)", args[0])
}

// apiTypes prints the record reference, all of it or the named records.
func apiTypes(e *Env, names []string) error {
	if len(names) == 0 {
		fmt.Fprint(e.Stdout, apipy.Types)
		return nil
	}
	blocks := strings.Split(apipy.Types, "\n\n")
	for _, n := range names {
		found := false
		for _, b := range blocks[1:] {
			if head, _, _ := strings.Cut(b, " "); strings.EqualFold(head, n) {
				fmt.Fprintln(e.Stdout, strings.TrimRight(b, "\n"))
				found = true
				break
			}
		}
		if !found {
			var all []string
			for _, b := range blocks[1:] {
				head, _, _ := strings.Cut(b, " ")
				all = append(all, head)
			}
			return fmt.Errorf("no record %q; the records are: %s", n, strings.Join(all, " "))
		}
	}
	return nil
}

// apiRun runs script (a path, or - for standard input) under python3 with
// the comebye module on its path, aimed at the CLI's Comebye. The token
// goes over an inherited pipe (fd 3), so it is in no environment, argument
// or file the script's children could see.
func apiRun(e *Env, script string, scriptArgs []string) error {
	py, err := exec.LookPath("python3")
	if err != nil {
		return errors.New("python3 is not on PATH: `comebye api run` needs Python 3.8 or newer (standard library only); install it (e.g. dnf install python3 or apt install python3). Nothing was run")
	}
	if script != "-" {
		if st, err := os.Stat(script); err != nil {
			return fmt.Errorf("script: %v", err)
		} else if st.IsDir() {
			return fmt.Errorf("script %s is a directory", script)
		}
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "comebye-api-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := writeModule(dir); err != nil {
		return err
	}

	env := apiEnv(os.Environ(), dir)
	env = append(env, "COMEBYE_API_URL="+c.base, "COMEBYE_API_PANE="+c.pane, "COMEBYE_API_HOST="+c.host)
	var tokenR *os.File
	if c.tokenErr != nil {
		msg := fmt.Sprintf("agent token: %v", c.tokenErr)
		if errors.Is(c.tokenErr, os.ErrNotExist) {
			msg = fmt.Sprintf("no agent token at %s: is Comebye running on this host? (set COMEBYE_STATE_DIR if it uses another state directory)", agenttoken.Path(StateDir(e.Getenv)))
		}
		env = append(env, "COMEBYE_API_TOKEN_ERROR="+msg)
	} else {
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		defer r.Close()
		// A token is far smaller than a pipe's buffer: the write cannot block.
		_, werr := io.WriteString(w, c.token)
		w.Close()
		if werr != nil {
			return werr
		}
		tokenR = r
		env = append(env, "COMEBYE_API_TOKEN_FD=3")
	}

	cmd := exec.Command(py, append([]string{script}, scriptArgs...)...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if tokenR != nil {
		cmd.ExtraFiles = []*os.File{tokenR} // fd 3
	}
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code > 0 {
			return exitStatus(code)
		}
		return exitStatus(1) // killed by a signal
	}
	return err
}

// writeModule writes the embedded comebye package into dir.
func writeModule(dir string) error {
	return fs.WalkDir(apipy.Module(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		b, err := fs.ReadFile(apipy.Module(), p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o600)
	})
}

// apiEnv is the script's environment: the caller's, with the module's dir
// first on PYTHONPATH, no stale COMEBYE_API_* and no bytecode files left in
// the temporary module dir.
func apiEnv(environ []string, dir string) []string {
	var out []string
	pythonPath := dir
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "COMEBYE_API_"), k == "PYTHONDONTWRITEBYTECODE":
			continue
		case k == "PYTHONPATH":
			if v != "" {
				pythonPath += string(os.PathListSeparator) + v
			}
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PYTHONPATH="+pythonPath, "PYTHONDONTWRITEBYTECODE=1")
}
