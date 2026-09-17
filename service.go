package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"wincron/internal/cron"
)

const serviceName = "wincron"

type cronService struct {
	crontabPath, logPath string
}

// startStopWaitHintMillis tells SCM how long a pending start or stop may
// take; it must exceed jobShutdownGrace or a graceful stop looks hung.
const startStopWaitHintMillis = 30000

const eventIDFatal = 1

// reportFatalToEventLog best-effort reports err to the Windows event log,
// the only visible trace when the service fails before wincron.log is usable.
func reportFatalToEventLog(err error) {
	elog, oerr := eventlog.Open(serviceName)
	if oerr != nil {
		return
	}
	defer func() { _ = elog.Close() }()
	_ = elog.Error(eventIDFatal, err.Error())
}

func (s *cronService) Execute(args []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: startStopWaitHintMillis}
	logger, closer, err := openLogger(s.logPath, false)
	if err != nil {
		reportFatalToEventLog(fmt.Errorf("opening log file %s: %w", s.logPath, err))
		return false, 1
	}
	defer func() { _ = closer.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cron.NewScheduler(s.crontabPath, logger).Run(ctx)
		close(done)
	}()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for req := range requests {
		switch req.Cmd {
		case svc.Interrogate:
			status <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending, WaitHint: startStopWaitHintMillis}
			cancel()
			<-done
			return false, 0
		}
	}
	cancel()
	<-done
	return false, 0
}

func runService(crontabPath, logPath string) error {
	return svc.Run(serviceName, &cronService{crontabPath: crontabPath, logPath: logPath})
}

func connectManager() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("cannot connect to service manager (run as administrator): %w", err)
	}
	return m, nil
}

// registerEventSource registers serviceName as an event log source; being
// registered already (a reinstall) is not an error.
func registerEventSource() error {
	err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("registering event source: %w", err)
	}
	return nil
}

// recoveryResetPeriodSeconds is how long without failures before SCM resets
// the failure count.
const recoveryResetPeriodSeconds = 24 * 60 * 60

// setRecoveryActions makes SCM restart the service on failure with
// increasing delays. Non-crash failures (a nonzero exit code, as when
// openLogger fails) must be opted into separately.
func setRecoveryActions(service *mgr.Service) error {
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := service.SetRecoveryActions(actions, recoveryResetPeriodSeconds); err != nil {
		return fmt.Errorf("setting recovery actions: %w", err)
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("enabling recovery for non-crash failures: %w", err)
	}
	return nil
}

func installService(exePath string) error {
	m, err := connectManager()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	service, err := m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: "wincron scheduler",
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return err
	}
	if err := registerEventSource(); err != nil {
		_ = service.Close()
		return err
	}
	if err := setRecoveryActions(service); err != nil {
		_ = service.Close()
		return err
	}
	return service.Close()
}

// uninstallService stops the service before deleting it: DeleteService on a
// running service only marks it for deletion, leaving it running and making a
// reinstall fail with ERROR_SERVICE_MARKED_FOR_DELETE.
func uninstallService() error {
	m, err := connectManager()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	service, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	if err := stopAndWait(service); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return err
	}
	if err := service.Delete(); err != nil {
		return err
	}
	_ = eventlog.Remove(serviceName)
	return nil
}

func startService() error {
	m, err := connectManager()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	service, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	return service.Start()
}

func stopService() error {
	m, err := connectManager()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	service, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	return stopAndWait(service)
}

// stopAndWait sends a stop control and polls until the service reports
// stopped. Stopping a service that is not running returns
// ERROR_SERVICE_NOT_ACTIVE.
func stopAndWait(service *mgr.Service) error {
	state, err := service.Control(svc.Stop)
	if err != nil {
		return err
	}
	if state.State == svc.Stopped {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for service to stop")
		case <-ticker.C:
			state, err = service.Query()
			if err != nil {
				return err
			}
			if state.State == svc.Stopped {
				return nil
			}
		}
	}
}
