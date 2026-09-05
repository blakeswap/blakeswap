package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/daemon"
)

// A subprocess exercises the real parent-PID watchdog and listener/vault cleanup.
// Its deliberately unavailable loopback endpoints never contact public services.
func TestDesktopSubprocess(t *testing.T) {
	role := os.Getenv("BLAKESWAP_PROCESS_TEST_ROLE")
	if role == "" {
		return
	}
	root := os.Getenv("BLAKESWAP_PROCESS_TEST_ROOT")
	if role == "parent" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDesktopSubprocess$")
		cmd.Env = append(os.Environ(), "BLAKESWAP_PROCESS_TEST_ROLE=wallet")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "wallet.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	if err := Run(ctx, root, os.Getppid()); err != nil {
		t.Fatal(err)
	}
}
func TestDesktopOwnedProcessLifecycle(t *testing.T) {
	for _, scenario := range []string{"terminate", "parent-death"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			settings := Defaults()
			env := environment(settings, "mainnet")
			cookie := filepath.Join(root, "rpc.cookie")
			if err := os.WriteFile(cookie, []byte("test:test"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, node := range env.Nodes {
				node.Kind = "rpc"
				node.Url = "http://127.0.0.1:1"
				node.Cookie = cookie
				node.CertificateSha256 = ""
			}
			if err := saveSettings(root, settings); err != nil {
				t.Fatal(err)
			}
			log, err := os.Create(filepath.Join(root, "process.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			role := "wallet"
			if scenario == "parent-death" {
				role = "parent"
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestDesktopSubprocess$")
			cmd.Env = append(os.Environ(), "BLAKESWAP_PROCESS_TEST_ROLE="+role, "BLAKESWAP_PROCESS_TEST_ROOT="+root)
			cmd.Stdout = log
			cmd.Stderr = log
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			wait := func(label string, condition func() bool) {
				t.Helper()
				until := time.Now().Add(10 * time.Second)
				for time.Now().Before(until) {
					if condition() {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				raw, _ := os.ReadFile(filepath.Join(root, "process.log"))
				t.Fatalf("%s: %s", label, raw)
			}
			manifest := filepath.Join(root, "runtime.json")
			wait("runtime startup", func() bool { _, err := os.Stat(manifest); return err == nil })
			raw, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			var endpoints map[string]api.Endpoint
			if err = json.Unmarshal(raw, &endpoints); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			status, err := api.Call(ctx, endpoints["alice"].Socket, daemon.Request{Method: "status"})
			if err != nil {
				t.Fatal(err)
			}
			if len(status) == 0 {
				t.Fatal("missing status")
			}
			if _, err = api.Call(ctx, endpoints["alice"].Socket, daemon.Request{Method: "settings.get"}); err != nil {
				t.Fatal("Settings unavailable while connecting", err)
			}
			childPID := cmd.Process.Pid
			if scenario == "parent-death" {
				wait("wallet PID", func() bool { _, err := os.Stat(filepath.Join(root, "wallet.pid")); return err == nil })
				raw, _ := os.ReadFile(filepath.Join(root, "wallet.pid"))
				childPID, err = strconv.Atoi(string(raw))
				if err != nil {
					t.Fatal(err)
				}
				if err = cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			} else if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			wait("runtime cleanup", func() bool { _, err := os.Stat(manifest); return os.IsNotExist(err) })
			wait("owned helper stopped", func() bool { return syscall.Kill(childPID, 0) == syscall.ESRCH })
			for _, endpoint := range endpoints {
				if _, err = os.Stat(endpoint.Socket); !os.IsNotExist(err) {
					t.Fatal("socket survived", err)
				}
				if _, err = os.Stat(endpoint.Socket + ".json"); !os.IsNotExist(err) {
					t.Fatal("credential survived", err)
				}
			}
			lock, err := os.OpenFile(filepath.Join(root, "desktop.lock"), os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal("wallet lock survived", err)
			}
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			t.Log(fmt.Sprintf("%s cleaned helper, runtime, sockets and wallet lock", scenario))
		})
	}
}
