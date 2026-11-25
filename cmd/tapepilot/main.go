package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/api"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/doctor"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/engine"
	"github.com/maxya/TapePilot/internal/jobs"
	"github.com/maxya/TapePilot/internal/notify"
	"github.com/maxya/TapePilot/internal/schedule"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/webui"
	"net/url"
)

const usage = `tapepilot [-config FILE] COMMAND
  serve | validate | version | healthcheck | doctor | qualification-check EVIDENCE.json
  tape-alerts CAPTURE --layout LAYOUT --qualification-ref REFERENCE --pdt 1
  physical-doctor --confirm-read-only --confirm-write-protected
  physical-snapshot LEGACY_ROOT MAX_FILES MAX_BYTES --confirm-read-only --confirm-write-protected
  sim-create CARTRIDGE CAPACITY_BYTES | sim-insert CARTRIDGE | sim-status
  initialize CARTRIDGE LABEL --confirm-synthetic
  enroll VOLUME_UUID | revoke VOLUME_UUID | seal VOLUME_UUID
  backup VOLUME_UUID SOURCE_ID [RELATIVE_SELECTION]
  verify VOLUME | inspect | rewind | unload
  jobs | volumes | run-one | cancel JOB | recheck JOB | snapshot FRESH_PATH
  import [EXPECTED_VOLUME] | legacy-import SOURCE LABEL --confirm
  admin bootstrap | admin reset
  archives VOLUME | members ARCHIVE | search TEXT | descriptor VOLUME | catalog-ndjson VOLUME | recovery-text VOLUME
  restore-preview ARCHIVE ROOT NEW_NAME (all | MEMBER_ID...)
  restore ARCHIVE ROOT NEW_NAME (all | MEMBER_ID...) [--legacy SOURCE]
Offline commands require exclusive ownership of the configured state directory.`

func main() {
	file := flag.String("config", "", "simulator configuration file")
	container := flag.Bool("container", false, "listen on a private container network")
	flag.Usage = func() { fmt.Fprintln(flag.CommandLine.Output(), usage) }
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *file, *container, flag.Args(), os.Stdout); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(ctx context.Context, file string, container bool, args []string, out io.Writer) (err error) {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	if args[0] == "tape-alerts" {
		return alertsCommand(args, out)
	}
	if args[0] == "qualification-check" {
		if len(args) != 2 {
			return errors.New(usage)
		}
		return checkQualification(args[1], out)
	}
	c := config.Default()
	if len(args) == 1 {
		switch args[0] {
		case "version":
			_, err := fmt.Fprintln(out, "TapePilot simulator development")
			return err
		case "healthcheck":
			client := http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get("http://127.0.0.1:8788/api/v1/health")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return errors.New("health check failed")
			}
			return nil
		}
	}
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		var e error
		c, e = config.Read(f)
		if err = errors.Join(e, f.Close()); err != nil {
			return err
		}
	}
	if args[0] == "physical-doctor" || args[0] == "physical-snapshot" {
		return physicalCommand(ctx, c, args, out)
	}
	if len(args) == 1 && args[0] == "doctor" {
		return json.NewEncoder(out).Encode(doctor.Inspect(ctx, c))
	}
	if len(args) == 1 && args[0] == "validate" {
		return c.CheckHost()
	}
	r, err := app.Open(ctx, c)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Store.Close()) }()
	services := r.Services()
	e, scheduler := services.Engine, services.Scheduler
	encode := json.NewEncoder(out).Encode
	if args[0] == "serve" && len(args) == 1 {
		return serve(ctx, container, r, services)
	}
	switch args[0] {
	case "import", "legacy-import", "restore-preview", "restore", "members", "archives", "search", "descriptor", "catalog-ndjson", "recovery-text":
		return catalogCommand(ctx, r, services, args, out, encode)
	case "admin":
		return adminCommand(r, args[1:])
	case "snapshot":
		if len(args) == 2 {
			return r.Store.Backup(ctx, args[1])
		}
	case "cancel":
		if len(args) == 2 {
			return scheduler.Cancel(ctx, args[1])
		}
	case "recheck":
		if len(args) == 2 {
			return scheduler.Recheck(ctx, args[1])
		}
	case "sim-create":
		if len(args) == 3 {
			n, err := strconv.ParseInt(args[2], 10, 64)
			if err != nil {
				return err
			}
			return r.Drive.Create(args[1], n, false)
		}
	case "sim-insert":
		if len(args) == 2 {
			return r.Insert(args[1])
		}
	case "jobs":
		if len(args) == 1 {
			j, err := r.Store.ListJobs(ctx, 100)
			if err != nil {
				return err
			}
			return encode(j)
		}
	case "volumes":
		if len(args) == 1 {
			v, err := r.Store.ListVolumes(ctx, 100)
			if err != nil {
				return err
			}
			return encode(v)
		}
	case "sim-status":
		if len(args) == 1 {
			l, err := r.Drive.Acquire(ctx)
			if err != nil {
				return err
			}
			s, err := l.Status(ctx)
			if err = errors.Join(err, l.Close()); err != nil {
				return err
			}
			return encode(s)
		}
	case "run-one":
		if len(args) == 1 {
			processed, err := scheduler.RunOne(ctx)
			if err != nil {
				return err
			}
			return encode(map[string]bool{"processed": processed})
		}
	case "seal":
		if len(args) == 2 {
			return r.Store.SetVolumeState(ctx, args[1], domain.Sealed)
		}
	case "revoke":
		if len(args) == 2 {
			return r.Store.RevokeEnrollment(ctx, args[1], "local-cli")
		}
	case "enroll":
		if len(args) == 2 {
			l, err := r.Drive.Acquire(ctx)
			if err != nil {
				return err
			}
			obs, err := l.Observe(ctx)
			if err = errors.Join(err, l.Close()); err != nil {
				return err
			}
			return r.Store.EnrollVolume(ctx, store.Enrollment{VolumeID: args[1], DriveID: obs.Drive, Profile: obs.Profile, OperatorRef: "local-cli"})
		}
	case "initialize", "backup", "verify", "inspect", "rewind", "unload":
		j := store.Job{ID: uuid.NewString()}
		var opts any
		switch args[0] {
		case "verify":
			if len(args) != 2 {
				return errors.New(usage)
			}
			j.Kind, j.VolumeID = domain.Verify, args[1]
			opts = struct{}{}
		case "inspect", "rewind", "unload":
			if len(args) != 1 {
				return errors.New(usage)
			}
			j.Kind = map[string]domain.Kind{"inspect": domain.Inspect, "rewind": domain.Rewind, "unload": domain.Unload}[args[0]]
			opts = struct{}{}
		case "initialize":
			if len(args) != 4 || args[3] != "--confirm-synthetic" || args[2] == "" || len(args[2]) > 256 {
				return errors.New(usage)
			}
			l, err := r.Drive.Acquire(ctx)
			if err != nil {
				return err
			}
			status, err := l.Status(ctx)
			if err = errors.Join(err, l.Close()); err != nil {
				return err
			}
			if status.CartridgeID != args[1] || status.Uncertain || status.CompletedFiles != 0 || status.WriteProtected {
				return errors.New("requires selected blank synthetic cartridge")
			}
			j.Kind, j.VolumeID = domain.Initialize, uuid.NewString()
			if err = r.Store.CreateVolume(ctx, store.Volume{ID: j.VolumeID, Kind: "MANAGED", Label: args[2], Generation: "simulator", State: domain.BlankCandidate}); err != nil {
				return err
			}
			e.InitializeArmed = true
			e.InitializeJobID = j.ID
			opts = engine.InitOptions{CartridgeID: args[1], Generation: status.Generation, Confirmed: true}
		case "backup":
			if len(args) != 3 && len(args) != 4 {
				return errors.New(usage)
			}
			var selection string
			if len(args) == 4 {
				selection = args[3]
			}
			if _, ok := c.Sources[args[2]]; !ok {
				return errors.New("unknown source ID")
			}
			j.Kind, j.VolumeID = domain.Backup, args[1]
			opts = engine.BackupOptions{SourceID: args[2], SourceLabel: args[2], Selection: selection}
		}
		return executeJob(ctx, r, scheduler, j, opts, encode)
	}
	return errors.New(usage)
}
func serve(ctx context.Context, container bool, r *app.Runtime, services app.Services) error {
	addr := "127.0.0.1:8788"
	if container {
		addr = "0.0.0.0:8788"
	}
	if !container && webui.Embedded() {
		u, e := url.Parse(r.Settings.PublicOrigin)
		if e != nil {
			return e
		}
		if u.Scheme == "http" {
			addr = u.Host
			if u.Port() == "" {
				addr = net.JoinHostPort(u.Hostname(), "80")
			}
		}
	}
	handler, err := api.NewConsole(r, services)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := http.Server{Addr: addr, Handler: webui.Wrap(handler), ReadHeaderTimeout: 4 * time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384,
		BaseContext: func(net.Listener) context.Context { return runCtx }}
	workers := 3
	if r.Settings.SMTP != nil {
		workers++
	}
	ended := make(chan error, workers)
	if r.Settings.SMTP != nil {
		go func() { ended <- notify.Run(runCtx, r.Store, *r.Settings.SMTP) }()
	}
	go func() { ended <- services.Scheduler.Run(runCtx) }()
	go func() { ended <- schedule.Run(runCtx, r, services.Scheduler.Wake) }()
	go func() { ended <- srv.ListenAndServe() }()
	first := <-ended
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	shutdownErr := srv.Shutdown(shutdownCtx)
	results := []error{first, shutdownErr}
	for i := 1; i < workers; i++ {
		results = append(results, <-ended)
	}
	for _, err := range results {
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func executeJob(ctx context.Context, r *app.Runtime, scheduler *jobs.Scheduler, j store.Job, opts any, encode func(any) error) error {
	var err error
	j.Options, err = json.Marshal(opts)
	if err != nil {
		return err
	}
	if _, err = r.Store.Enqueue(ctx, j); err != nil {
		return err
	}
	for {
		current, err := r.Store.GetJob(ctx, j.ID)
		if err != nil {
			return err
		}
		if current.State != domain.Queued {
			if err = encode(current); err != nil {
				return err
			}
			if current.State != domain.Succeeded {
				return fmt.Errorf("job %s: %s", current.ID, current.State)
			}
			return nil
		}
		processed, err := scheduler.RunOne(ctx)
		if err != nil {
			return err
		}
		if !processed {
			return errors.New("scheduler did not claim queued job")
		}
	}
}
