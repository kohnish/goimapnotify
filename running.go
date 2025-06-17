package main

// This file is part of goimapnotify
// Copyright (C) 2017-2025	Jorge Javier Araya Navarro

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

import (
	"bytes"
	"fmt"
	"os/exec"
	"sync"
	"text/template"
	"time"
	"errors"
	"runtime"
	"strconv"
	"github.com/sirupsen/logrus"
)

// This is terrible, slow, and should never be used.
func goid() (int, error) {
    buf := make([]byte, 32)
    n := runtime.Stack(buf, false)
    buf = buf[:n]
    // goroutine 1 [running]: ...

    buf, ok := bytes.CutPrefix(buf, goroutinePrefix)
    if !ok {
        return 0, errBadStack
    }

    i := bytes.IndexByte(buf, ' ')
    if i < 0 {
        return 0, errBadStack
    }

    return strconv.Atoi(string(buf[:i]))
}

type RecursiveMutex struct {
	mutex            sync.Mutex
	internalMutex    sync.Mutex
	currentGoRoutine int
	lockCount        uint64
}

func (rm *RecursiveMutex) Lock() {
	goRoutineID, _ := goid()

	for {
		rm.internalMutex.Lock()
		if rm.currentGoRoutine == 0 {
			rm.currentGoRoutine = goRoutineID
			break
		} else if rm.currentGoRoutine == goRoutineID {
			break
		} else {
			rm.internalMutex.Unlock()
			time.Sleep(time.Millisecond)
			continue
		}
	}
	rm.lockCount++
	if rm.lockCount > 1 {
		logrus.Errorf("Fix double locking [%d]!", rm.lockCount)
	}
	rm.internalMutex.Unlock()
}

func (rm *RecursiveMutex) Unlock() {
	rm.internalMutex.Lock()
	rm.lockCount--
	if rm.lockCount == 0 {
		rm.currentGoRoutine = 0
	}
	rm.internalMutex.Unlock()
}


var (
	goroutinePrefix = []byte("goroutine ")
	errBadStack     = errors.New("invalid runtime.Stack output")
	s RecursiveMutex

)

type RunningBox struct {
	debug bool
	wait  int
	/*
	 * Use map to create a different timer for each
	 * username-mailbox combination
	 */
	timer  sync.Map
	config map[string]NotifyConfig
}

func NewRunningBox(debug bool, wait int) *RunningBox {
	return &RunningBox{
		debug:  debug,
		wait:   wait,
		timer:  sync.Map{},
		config: make(map[string]NotifyConfig),
	}
}

func (r *RunningBox) schedule(rsp IDLEEvent, done <-chan struct{}, queue chan IDLEEvent) {
	l := logrus.WithField("alias", rsp.Alias).WithField("mailbox", rsp.Mailbox)
	if shouldSkip(rsp.box) {
		l.Warnf("No command for %q, skipping scheduling...", rsp.Reason)
		return
	}

	key := rsp.Alias + rsp.Mailbox
	wait := time.Duration(r.wait) * time.Second
	when := time.Now().Add(wait).Format(time.RFC850)
	format := fmt.Sprintf("%%s syncing %q for %s (%s in the future)", rsp.Reason, when, wait)

	s.Lock()
	value, exists := r.timer.LoadOrStore(key, time.NewTimer(wait))
	wristwatch, ok := value.(*time.Timer)
	if !ok {
		l.Fatal("stored value isn't *time.Timer")
	}
	main := true // main is true for the goroutine that will run sync
	if exists {
		// Stop should be called before Reset according to go docs
		if wristwatch.Stop() {
			main = false // stopped running timer -> main is another goroutine
		}
		wristwatch.Reset(wait)
		r.timer.Store(key, wristwatch)
	}

	if main {
		l.Infof(format, "scheduled")
		select {
		case <-wristwatch.C:
			queue <- rsp
		case <-done:
			// just get out
		}
	} else {
		l.Infof(format, "rescheduled")
	}
	s.Unlock()
}

func (r *RunningBox) run(rsp IDLEEvent) error {
	l := logrus.WithField("alias", rsp.Alias).WithField("mailbox", rsp.Mailbox)
	if r.debug {
		l.Infoln("Running synchronization...")
	}

	switch rsp.Reason {
	case NEWMAIL:
		err := prepareAndRun(rsp.box.OnNewMail, rsp.box.OnNewMailPost, rsp)
		if err != nil {
			return err
		}
	case DELETEDMAIL:
		err := prepareAndRun(rsp.box.OnDeletedMail, rsp.box.OnDeletedMailPost, rsp)
		if err != nil {
			return err
		}
	default:
		l.WithField("reason", rsp.Reason).Error("unknown reason value, ignoring...")
	}

	return nil
}

func prepareAndRun(on, onpost string, event IDLEEvent) error {
	callKind := "New"
	if event.Reason == DELETEDMAIL {
		callKind = "Deleted"
	}

	if on == "SKIP" || on == "" {
		return nil
	}

	bufOn := bytes.NewBuffer(nil)
	tOn, err := template.New("run").Parse(on)
	if err != nil {
		return fmt.Errorf("cannot compile template for 'on' command, error: %w", err)
	}
	err = tOn.Execute(bufOn, event)
	if err != nil {
		return fmt.Errorf("there was an error while executing the template, error: %w", err)
	}

	call := PrepareCommand(bufOn.String(), event)
	out, err := call.Output()
	if err != nil {
		exiterr, ok := err.(*exec.ExitError)
		if ok {
			logrus.Errorf("stderror: %q", string(exiterr.Stderr))
		}
		return fmt.Errorf("On%sMail command failed: %v", callKind, err)
	}
	logrus.Infof("stdout: %q", string(out))

	if onpost == "SKIP" || onpost == "" {
		return nil
	}

	bufOnPost := bytes.NewBuffer(nil)
	tOnPost, err := template.New("run").Parse(onpost)
	if err != nil {
		return fmt.Errorf("cannot compile template for 'onPost' command, error: %w", err)
	}
	err = tOnPost.Execute(bufOnPost, event)
	if err != nil {
		return fmt.Errorf("there was an error while executing the template, error: %w", err)
	}

	call = PrepareCommand(bufOnPost.String(), event)
	out, err = call.Output()
	if err != nil {
		exiterr, ok := err.(*exec.ExitError)
		if ok {
			logrus.Errorf("stderror: %q", string(exiterr.Stderr))
		}
		return fmt.Errorf("On%sMailPost command failed: %v", callKind, err)
	}
	logrus.Infof("stdout: %q", string(out))

	return nil
}

func shouldSkip(b Box) bool {
	switch b.Reason {
	case NEWMAIL:
		return b.OnNewMail == "" || b.OnNewMail == "SKIP"
	case DELETEDMAIL:
		return b.OnDeletedMail == "" || b.OnDeletedMail == "SKIP"
	}

	return false
}
