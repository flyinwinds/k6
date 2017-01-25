/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2016 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/ghodss/yaml"
	"github.com/loadimpact/k6/api"
	"github.com/loadimpact/k6/js"
	"github.com/loadimpact/k6/lib"
	"github.com/loadimpact/k6/simple"
	"github.com/loadimpact/k6/stats"
	"github.com/loadimpact/k6/stats/influxdb"
	"github.com/loadimpact/k6/stats/json"
	"github.com/loadimpact/k6/ui"
	"gopkg.in/urfave/cli.v1"
)

var commandRun = cli.Command{
	Name:      "run",
	Usage:     "Starts running a load test",
	ArgsUsage: "url|filename",
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name:  "vus, u",
			Usage: "virtual users to simulate",
			Value: 10,
		},
		cli.Int64Flag{
			Name:  "max, m",
			Usage: "max number of virtual users, if more than --vus",
		},
		cli.DurationFlag{
			Name:  "duration, d",
			Usage: "test duration, 0 to run until cancelled",
			Value: 10 * time.Second,
		},
		cli.Float64Flag{
			Name:  "acceptance, a",
			Usage: "acceptable margin of error before failing the test",
			Value: 0.0,
		},
		cli.BoolFlag{
			Name:  "paused, p",
			Usage: "start test in a paused state",
		},
		cli.StringFlag{
			Name:  "type, t",
			Usage: "input type, one of: auto, url, js",
			Value: "auto",
		},
		cli.BoolFlag{
			Name:  "linger, l",
			Usage: "linger after test completion",
		},
		cli.BoolFlag{
			Name:  "abort-on-taint",
			Usage: "abort immediately if the test gets tainted",
		},
		cli.Int64Flag{
			Name:  "max-redirects",
			Usage: "follow at most n redirects",
		},
		cli.StringFlag{
			Name:  "out, o",
			Usage: "output metrics to an external data store",
		},
		cli.StringSliceFlag{
			Name:  "config, c",
			Usage: "read additional config files",
		},
	},
	Action: actionRun,
	Description: `Run starts a load test.

   This is the main entry point to k6, and will do two things:
   
   - Construct an Engine and provide it with a Runner, depending on the first
     argument and the --type flag, which is used to execute the test.
   
   - Start an a web server on the address specified by the global --address
     flag, which serves a web interface and a REST API for remote control.
   
   For ease of use, you may also pass initial status parameters (vus, max,
   duration) to 'run', which will be applied through a normal API call.`,
}

var commandInspect = cli.Command{
	Name:      "inspect",
	Aliases:   []string{"i"},
	Usage:     "Merges and prints test configuration",
	ArgsUsage: "url|filename",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "type, t",
			Usage: "input type, one of: auto, url, js",
			Value: "auto",
		},
		cli.StringSliceFlag{
			Name:  "config, c",
			Usage: "read additional config files",
		},
	},
	Action: actionInspect,
}

func looksLikeURL(str []byte) bool {
	// Try to parse string as URL
	u, err := url.Parse(strings.TrimSpace(string(str)))
	if err != nil || u.Scheme == "" {
		return false
	}
	return true
}

func getSrcData(arg, t string) (*lib.SourceData, error) {
	srcdata := []byte("")
	srctype := t
	filename := arg
	var err error
	const cmdline = "[cmdline]"
	// special case name “-“ will always cause src data to be read from file STDIN
	if arg == "-" {
		srcdata, err = ioutil.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
	} else {
		// Deduce how to get src data
		switch srctype {
		case lib.TypeAuto:
			if looksLikeURL([]byte(arg)) { // always try to parse as URL string first
				srcdata = []byte(arg)
				srctype = lib.TypeURL
				filename = cmdline
			} else {
				// Otherwise, check if it is a file name and we can load the file
				srcdata, err = ioutil.ReadFile(arg)
				if err != nil { // if we fail to open file, we assume the arg is JS code
					srcdata = []byte(arg)
					srctype = lib.TypeJS
					filename = cmdline
				}
			}
		case lib.TypeURL:
			// We try to use lib.TypeURL args as URLs directly first
			if looksLikeURL([]byte(arg)) {
				srcdata = []byte(arg)
				filename = cmdline
			} else { // if that didn’t work, we try to load a file with URLs
				srcdata, err = ioutil.ReadFile(arg)
				if err != nil {
					return nil, err
				}
			}
		case lib.TypeJS:
			// lib.TypeJS args we try to use as file names first
			srcdata, err = ioutil.ReadFile(arg)
			if err != nil { // and if that didn’t work, we assume the arg itself is JS code
				srcdata = []byte(arg)
				filename = cmdline
			}
		}
	}
	// Now we should have some src data and in most cases a type also. If we
	// don’t have a type it means we read from STDIN or from a file and the user
	// specified type == lib.TypeAuto. This means we need to try and auto-detect type
	if srctype == lib.TypeAuto {
		if looksLikeURL(srcdata) {
			srctype = lib.TypeURL
		} else {
			srctype = lib.TypeJS
		}
	}
	src := &lib.SourceData{
		SrcData:  srcdata,
		Filename: filename,
		SrcType:  srctype,
	}
	return src, nil
}

func makeRunner(srcdata *lib.SourceData) (lib.Runner, error) {
	switch srcdata.SrcType {
	case "":
		return nil, errors.New("Invalid type specified, see --help")
	case lib.TypeURL:
		u, err := url.Parse(strings.TrimSpace(string(srcdata.SrcData)))
		if err != nil || u.Scheme == "" {
			return nil, errors.New("Failed to parse URL")
		}
		r, err := simple.New(u)
		if err != nil {
			return nil, err
		}
		return r, err
	case lib.TypeJS:
		rt, err := js.New()
		if err != nil {
			return nil, err
		}
		exports, err := rt.Load(srcdata)
		if err != nil {
			return nil, err
		}
		r, err := js.NewRunner(rt, exports)
		if err != nil {
			return nil, err
		}
		return r, nil
	default:
		return nil, errors.New("Invalid type specified, see --help")
	}
}

func parseCollectorString(s string) (t, p string, err error) {
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return "", "", errors.New("Malformed output; must be in the form 'type=url'")
	}

	return parts[0], parts[1], nil
}

func makeCollector(s string) (stats.Collector, error) {
	t, p, err := parseCollectorString(s)
	if err != nil {
		return nil, err
	}

	switch t {
	case "influxdb":
		return influxdb.New(p)
	case "json":
		return json.New(p)
	default:
		return nil, errors.New("Unknown output type: " + t)
	}
}

func actionRun(cc *cli.Context) error {
	wg := sync.WaitGroup{}

	args := cc.Args()
	if len(args) != 1 {
		return cli.NewExitError("Wrong number of arguments!", 1)
	}

	// Collect CLI arguments, most (not all) relating to options.
	addr := cc.GlobalString("address")
	out := cc.String("out")
	opts := lib.Options{
		Paused:       cliBool(cc, "paused"),
		VUs:          cliInt64(cc, "vus"),
		VUsMax:       cliInt64(cc, "max"),
		Duration:     cliDuration(cc, "duration"),
		Linger:       cliBool(cc, "linger"),
		AbortOnTaint: cliBool(cc, "abort-on-taint"),
		Acceptance:   cliFloat64(cc, "acceptance"),
		MaxRedirects: cliInt64(cc, "max-redirects"),
	}

	// Make the Runner, extract script-defined options.
	arg := args[0]
	runnerType := cc.String("type")
	srcdata, err := getSrcData(arg, runnerType)
	if err != nil {
		log.WithError(err).Error("Couldn't create a runner")
		return err
	}
	runner, err := makeRunner(srcdata)
	if err != nil {
		log.WithError(err).Error("Couldn't create a runner")
		return err
	}
	opts = opts.Apply(runner.GetOptions())

	// Read config files.
	for _, filename := range cc.StringSlice("config") {
		data, err := ioutil.ReadFile(filename)
		if err != nil {
			return cli.NewExitError(err.Error(), 1)
		}

		var configOpts lib.Options
		if err := yaml.Unmarshal(data, &configOpts); err != nil {
			return cli.NewExitError(err.Error(), 1)
		}
		opts = opts.Apply(configOpts)
	}

	// CLI options have defaults, which are set as invalid, but have potentially nonzero values.
	// Flipping the Valid flag for all invalid options thus applies all defaults.
	if !opts.VUsMax.Valid {
		opts.VUsMax.Int64 = opts.VUs.Int64
	}
	opts = opts.SetAllValid(true)
	runner.ApplyOptions(opts)

	// Make the metric collector, if requested.
	var collector stats.Collector
	collectorString := "-"
	if out != "" {
		c, err := makeCollector(out)
		if err != nil {
			log.WithError(err).Error("Couldn't create output")
			return err
		}
		collector = c
		collectorString = fmt.Sprint(collector)
	}

	// Make the Engine
	engine, err := lib.NewEngine(runner)
	if err != nil {
		log.WithError(err).Error("Couldn't create the engine")
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	engine.Collector = collector

	// Run the engine.
	wg.Add(1)
	go func() {
		defer func() {
			log.Debug("Engine terminated")
			wg.Done()
		}()
		log.Debug("Starting engine...")
		if err := engine.Run(ctx); err != nil {
			log.WithError(err).Error("Engine Error")
		}
		cancel()
	}()

	// Start the API server in the background.
	go func() {
		if err := api.ListenAndServe(addr, engine); err != nil {
			log.WithError(err).Error("Couldn't start API server!")
		}
	}()

	// Print the banner!
	fmt.Printf("Welcome to k6 v%s!\n", cc.App.Version)
	fmt.Printf("\n")
	fmt.Printf("  execution: local\n")
	fmt.Printf("     output: %s\n", collectorString)
	fmt.Printf("     script: %s (%s)\n", srcdata.Filename, srcdata.SrcType)
	fmt.Printf("             ↳ duration: %s\n", opts.Duration.String)
	fmt.Printf("             ↳ vus: %d, max: %d\n", opts.VUs.Int64, opts.VUsMax.Int64)
	fmt.Printf("\n")
	fmt.Printf("  web ui: http://%s/\n", addr)
	fmt.Printf("\n")

	progressBar := ui.ProgressBar{Width: 60}
	fmt.Printf(" starting %s -- / --\r", progressBar.String())

	// Wait for a signal or timeout before shutting down
	signals := make(chan os.Signal)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	ticker := time.NewTicker(10 * time.Millisecond)

loop:
	for {
		select {
		case <-ticker.C:
			statusString := "running"
			if !engine.Status.Running.Bool {
				if engine.IsRunning() {
					statusString = "paused"
				} else {
					statusString = "stopping"
				}
			}

			atTime := time.Duration(engine.Status.AtTime.Int64)
			totalTime, finite := engine.TotalTime()
			progress := 0.0
			if finite {
				progress = float64(atTime) / float64(totalTime)
			}

			progressBar.Progress = progress
			fmt.Printf("%10s %s %10s / %s\r",
				statusString,
				progressBar.String(),
				roundDuration(atTime, 100*time.Millisecond),
				roundDuration(totalTime, 100*time.Millisecond),
			)
		case <-ctx.Done():
			log.Debug("Engine terminated; shutting down...")
			break loop
		case sig := <-signals:
			log.WithField("signal", sig).Debug("Signal received; shutting down...")
			break loop
		}
	}

	// Shut down the API server and engine.
	cancel()
	wg.Wait()

	// Test done, leave that status as the final progress bar!
	atTime := time.Duration(engine.Status.AtTime.Int64)
	progressBar.Progress = 1.0
	fmt.Printf("      done %s %10s / %s\n",
		progressBar.String(),
		roundDuration(atTime, 100*time.Millisecond),
		roundDuration(atTime, 100*time.Millisecond),
	)
	fmt.Printf("\n")

	// Print groups.
	var printGroup func(g *lib.Group, level int)
	printGroup = func(g *lib.Group, level int) {
		indent := strings.Repeat("  ", level)

		if g.Name != "" && g.Parent != nil {
			fmt.Printf("%s█ %s\n", indent, g.Name)
		}

		if len(g.Checks) > 0 {
			if g.Name != "" && g.Parent != nil {
				fmt.Printf("\n")
			}
			for _, check := range g.Checks {
				icon := "✓"
				if check.Fails > 0 {
					icon = "✗"
				}
				fmt.Printf("%s  %s %2.2f%% - %s\n",
					indent,
					icon,
					100*(float64(check.Passes)/float64(check.Passes+check.Fails)),
					check.Name,
				)
			}
			fmt.Printf("\n")
		}
		if len(g.Groups) > 0 {
			if g.Name != "" && g.Parent != nil && len(g.Checks) > 0 {
				fmt.Printf("\n")
			}
			for _, g := range g.Groups {
				printGroup(g, level+1)
			}
		}
	}

	groups := engine.Runner.GetGroups()
	for _, g := range groups {
		if g.Parent != nil {
			continue
		}
		printGroup(g, 1)
	}

	// Sort and print metrics.
	metrics := make(map[string]*stats.Metric, len(engine.Metrics))
	metricNames := make([]string, 0, len(engine.Metrics))
	for m, _ := range engine.Metrics {
		metrics[m.Name] = m
		metricNames = append(metricNames, m.Name)
	}
	sort.Strings(metricNames)

	for _, name := range metricNames {
		m := metrics[name]
		m.Sample = engine.Metrics[m].Format()
		val := metrics[name].Humanize()
		if val == "0" {
			continue
		}
		icon := " "
		for _, threshold := range engine.Thresholds[name] {
			icon = "✓"
			if threshold.Failed {
				icon = "✗"
				break
			}
		}
		fmt.Printf("  %s %s: %s\n", icon, name, val)
	}

	if engine.Status.Tainted.Bool {
		return cli.NewExitError("", 99)
	}
	return nil
}

func actionInspect(cc *cli.Context) error {
	args := cc.Args()
	if len(args) != 1 {
		return cli.NewExitError("Wrong number of arguments!", 1)
	}
	arg := args[0]
	t := cc.String("type")
	srcdata, err := getSrcData(arg, t)
	if err != nil {
		return err
	}

	var opts lib.Options
	switch srcdata.SrcType {
	case lib.TypeJS:
		r, err := js.New()
		if err != nil {
			return cli.NewExitError(err.Error(), 1)
		}

		if _, err := r.Load(srcdata); err != nil {
			return cli.NewExitError(err.Error(), 1)
		}
		opts = opts.Apply(r.Options)
	}

	for _, filename := range cc.StringSlice("config") {
		data, err := ioutil.ReadFile(filename)
		if err != nil {
			return cli.NewExitError(err.Error(), 1)
		}

		var configOpts lib.Options
		if err := yaml.Unmarshal(data, &configOpts); err != nil {
			return cli.NewExitError(err.Error(), 1)
		}
		opts = opts.Apply(configOpts)
	}

	return dumpYAML(opts)
}
