/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nfdworker

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

var (
	stdoutLogger = log.New(os.Stdout, "", log.LstdFlags)
	stderrLogger = log.New(os.Stderr, "", log.LstdFlags)
	nodeName     = os.Getenv("NODE_NAME")
)

// Global config
type NFDConfig struct {
	Core    coreConfig
	Sources sourcesConfig
}

type coreConfig struct {
	NoPublish bool
}

type sourcesConfig map[string]source.Config

// Labels are a Kubernetes representation of discovered features.
type Labels map[string]string

// Command line arguments
type Args struct {
	LabelWhiteList     string
	CaFile             string
	CertFile           string
	KeyFile            string
	ConfigFile         string
	Options            string
	Oneshot            bool
	Server             string
	ServerNameOverride string
	SleepInterval      time.Duration
	Sources            []string
	// Deprecated options that should be set via the config file
	NoPublish *bool
}

type NfdWorker interface {
	Run() error
}

type nfdWorker struct {
	args           Args
	clientConn     *grpc.ClientConn
	client         pb.LabelerClient
	configFilePath string
	config         *NFDConfig
	sources        []source.FeatureSource
	labelWhiteList *regexp.Regexp
}

// Create new NfdWorker instance.
func NewNfdWorker(args Args) (NfdWorker, error) {
	nfd := &nfdWorker{
		args:    args,
		config:  &NFDConfig{},
		sources: []source.FeatureSource{},
	}

	if args.ConfigFile != "" {
		nfd.configFilePath = filepath.Clean(args.ConfigFile)
	}

	if args.SleepInterval > 0 && args.SleepInterval < time.Second {
		stderrLogger.Printf("WARNING: too short sleep-intervall specified (%s), forcing to 1s", args.SleepInterval.String())
		args.SleepInterval = time.Second
	}

	// Check TLS related args
	if args.CertFile != "" || args.KeyFile != "" || args.CaFile != "" {
		if args.CertFile == "" {
			return nfd, fmt.Errorf("--cert-file needs to be specified alongside --key-file and --ca-file")
		}
		if args.KeyFile == "" {
			return nfd, fmt.Errorf("--key-file needs to be specified alongside --cert-file and --ca-file")
		}
		if args.CaFile == "" {
			return nfd, fmt.Errorf("--ca-file needs to be specified alongside --cert-file and --key-file")
		}
	}

	// Figure out active sources
	allSources := []source.FeatureSource{
		&cpu.Source{},
		&iommu.Source{},
		&kernel.Source{},
		&memory.Source{},
		&network.Source{},
		&pci.Source{},
		&storage.Source{},
		&system.Source{},
		&usb.Source{},
		&custom.Source{},
		// local needs to be the last source so that it is able to override
		// labels from other sources
		&local.Source{},
	}

	// Determine enabled feature
	if len(args.Sources) == 1 && args.Sources[0] == "all" {
		nfd.sources = allSources
	} else {
		// Add fake source which is only meant for testing. It will be enabled
		// only if listed explicitly.
		allSources = append(allSources, &fake.Source{})
		allSources = append(allSources, &panicfake.Source{})

		sourceWhiteList := map[string]struct{}{}
		for _, s := range args.Sources {
			sourceWhiteList[strings.TrimSpace(s)] = struct{}{}
		}

		nfd.sources = []source.FeatureSource{}
		for _, s := range allSources {
			if _, enabled := sourceWhiteList[s.Name()]; enabled {
				nfd.sources = append(nfd.sources, s)
				delete(sourceWhiteList, s.Name())
			}
		}
		if len(sourceWhiteList) > 0 {
			names := make([]string, 0, len(sourceWhiteList))
			for n := range sourceWhiteList {
				names = append(names, n)
			}
			stderrLogger.Printf("WARNING: skipping unknown source(s) %q specified in --sources", strings.Join(names, ", "))
		}
	}

	// Compile labelWhiteList regex
	var err error
	nfd.labelWhiteList, err = regexp.Compile(args.LabelWhiteList)
	if err != nil {
		return nfd, fmt.Errorf("error parsing label whitelist regex (%s): %s", args.LabelWhiteList, err)
	}

	return nfd, nil
}

func addConfigWatch(path string) (*fsnotify.Watcher, map[string]struct{}, error) {
	paths := make(map[string]struct{})

	// Create watcher
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return w, paths, fmt.Errorf("failed to create fsnotify watcher: %v", err)
	}

	// Add watches for all directory components so that we catch e.g. renames
	// upper in the tree
	added := false
	for p := path; ; p = filepath.Dir(p) {

		if err := w.Add(p); err != nil {
			stdoutLogger.Printf("failed to add fsnotify watch for %q: %v", p, err)
		} else {
			stdoutLogger.Printf("added fsnotify watch %q", p)
			added = true
		}

		paths[p] = struct{}{}
		if filepath.Dir(p) == p {
			break
		}
	}

	if !added {
		// Want to be sure that we watch something
		return w, paths, fmt.Errorf("failed to add any watch")
	}
	return w, paths, nil
}

func newDefaultConfig() *NFDConfig {
	return &NFDConfig{
		Core: coreConfig{},
	}
}

// Run NfdWorker client. Returns if a fatal error is encountered, or, after
// one request if OneShot is set to 'true' in the worker args.
func (w *nfdWorker) Run() error {
	stdoutLogger.Printf("Node Feature Discovery Worker %s", version.Get())
	stdoutLogger.Printf("NodeName: '%s'", nodeName)

	// Create watcher for config file and read initial configuration
	configWatch, paths, err := addConfigWatch(w.configFilePath)
	if err != nil {
		return err
	}
	w.configure(w.configFilePath, w.args.Options)

	// Connect to NFD master
	err = w.connect()
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	defer w.disconnect()

	labelTrigger := time.After(0)
	var configTrigger <-chan time.Time
	for {
		select {
		case <-labelTrigger:
			// Get the set of feature labels.
			labels := createFeatureLabels(w.sources, w.labelWhiteList)

			// Update the node with the feature labels.
			if w.client != nil {
				err := advertiseFeatureLabels(w.client, labels)
				if err != nil {
					return fmt.Errorf("failed to advertise labels: %s", err.Error())
				}
			}

			if w.args.Oneshot {
				return nil
			}

			if w.args.SleepInterval > 0 {
				labelTrigger = time.After(w.args.SleepInterval)
			}

		case e := <-configWatch.Events:
			name := filepath.Clean(e.Name)

			// If any of our paths (directories or the file itself) change
			if _, ok := paths[name]; ok {
				stdoutLogger.Printf("fsnotify event in %q detected, reconfiguring fsnotify and reloading configuration", name)

				// Blindly remove existing watch and add a new one
				if err := configWatch.Close(); err != nil {
					stderrLogger.Printf("WARNING: failed to close fsnotify watcher: %v", err)
				}
				configWatch, paths, err = addConfigWatch(w.configFilePath)
				if err != nil {
					return err
				}

				// Rate limiter. In certain filesystem operations we get
				// numerous events in quick succession and we only want one
				// config re-load
				configTrigger = time.After(time.Second)
			}

		case e := <-configWatch.Errors:
			stderrLogger.Printf("ERROR: config file watcher error: %v", e)

		case <-configTrigger:
			w.configure(w.configFilePath, w.args.Options)
			// Manage connection to master
			if w.config.Core.NoPublish {
				w.disconnect()
			} else if w.clientConn == nil {
				if err := w.connect(); err != nil {
					return err
				}
			}
			// Always re-label after a re-config event. This way the new config
			// comes into effect even if the sleep interval is long (or infinite)
			labelTrigger = time.After(0)
		}
	}
}

// connect creates a client connection to the NFD master
func (w *nfdWorker) connect() error {
	// Return a dummy connection in case of dry-run
	if w.config.Core.NoPublish {
		return nil
	}

	// Check that if a connection already exists
	if w.clientConn != nil {
		return fmt.Errorf("client connection already exists")
	}

	// Dial and create a client
	dialCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dialOpts := []grpc.DialOption{grpc.WithBlock()}
	if w.args.CaFile != "" || w.args.CertFile != "" || w.args.KeyFile != "" {
		// Load client cert for client authentication
		cert, err := tls.LoadX509KeyPair(w.args.CertFile, w.args.KeyFile)
		if err != nil {
			return fmt.Errorf("failed to load client certificate: %v", err)
		}
		// Load CA cert for server cert verification
		caCert, err := ioutil.ReadFile(w.args.CaFile)
		if err != nil {
			return fmt.Errorf("failed to read root certificate file: %v", err)
		}
		caPool := x509.NewCertPool()
		if ok := caPool.AppendCertsFromPEM(caCert); !ok {
			return fmt.Errorf("failed to add certificate from '%s'", w.args.CaFile)
		}
		// Create TLS config
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      caPool,
			ServerName:   w.args.ServerNameOverride,
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		dialOpts = append(dialOpts, grpc.WithInsecure())
	}
	conn, err := grpc.DialContext(dialCtx, w.args.Server, dialOpts...)
	if err != nil {
		return err
	}
	w.clientConn = conn
	w.client = pb.NewLabelerClient(conn)

	return nil
}

// disconnect closes the connection to NFD master
func (w *nfdWorker) disconnect() {
	if w.clientConn != nil {
		w.clientConn.Close()
	}
	w.clientConn = nil
	w.client = nil
}

// Parse configuration options
func (w *nfdWorker) configure(filepath string, overrides string) {
	// Create a new default config
	c := newDefaultConfig()
	c.Sources = make(map[string]source.Config, len(w.sources))
	for _, s := range w.sources {
		c.Sources[s.Name()] = s.NewConfig()
	}

	// Try to read and parse config file
	data, err := ioutil.ReadFile(filepath)
	if err != nil {
		stderrLogger.Printf("Failed to read config file: %s", err)
	} else {
		err = yaml.Unmarshal(data, c)
		if err != nil {
			stderrLogger.Printf("Failed to parse config file: %s", err)
		} else {
			stdoutLogger.Printf("Configuration successfully loaded from %q", filepath)
		}
	}

	// Parse config overrides
	err = yaml.Unmarshal([]byte(overrides), c)
	if err != nil {
		stderrLogger.Printf("Failed to parse --options: %s", err)
	}

	if w.args.NoPublish != nil {
		c.Core.NoPublish = *w.args.NoPublish
	}

	w.config = c

	// (Re-)configure all sources
	for _, s := range w.sources {
		s.SetConfig(c.Sources[s.Name()])
	}
}

// createFeatureLabels returns the set of feature labels from the enabled
// sources and the whitelist argument.
func createFeatureLabels(sources []source.FeatureSource, labelWhiteList *regexp.Regexp) (labels Labels) {
	labels = Labels{}

	// Do feature discovery from all configured sources.
	for _, source := range sources {
		labelsFromSource, err := getFeatureLabels(source, labelWhiteList)
		if err != nil {
			stderrLogger.Printf("discovery failed for source [%s]: %s", source.Name(), err.Error())
			stderrLogger.Printf("continuing ...")
			continue
		}

		for name, value := range labelsFromSource {
			// Log discovered feature.
			stdoutLogger.Printf("%s = %s", name, value)
			labels[name] = value
		}
	}
	return labels
}

// getFeatureLabels returns node labels for features discovered by the
// supplied source.
func getFeatureLabels(source source.FeatureSource, labelWhiteList *regexp.Regexp) (labels Labels, err error) {
	defer func() {
		if r := recover(); r != nil {
			stderrLogger.Printf("panic occurred during discovery of source [%s]: %v", source.Name(), r)
			err = fmt.Errorf("%v", r)
		}
	}()

	labels = Labels{}
	features, err := source.Discover()
	if err != nil {
		return nil, err
	}

	// Prefix for labels in the default namespace
	prefix := source.Name() + "-"
	switch source.(type) {
	case *local.Source:
		// Do not prefix labels from the hooks
		prefix = ""
	}

	for k, v := range features {
		// Split label name into namespace and name compoents. Use dummy 'ns'
		// default namespace because there is no function to validate just
		// the name part
		split := strings.SplitN(k, "/", 2)

		label := prefix + split[0]
		nameForValidation := "ns/" + label
		nameForWhiteListing := label

		if len(split) == 2 {
			label = k
			nameForValidation = label
			nameForWhiteListing = split[1]
		}

		// Validate label name.
		errs := validation.IsQualifiedName(nameForValidation)
		if len(errs) > 0 {
			stderrLogger.Printf("Ignoring invalid feature name '%s': %s", label, errs)
			continue
		}

		value := fmt.Sprintf("%v", v)
		// Validate label value
		errs = validation.IsValidLabelValue(value)
		if len(errs) > 0 {
			stderrLogger.Printf("Ignoring invalid feature value %s=%s: %s", label, value, errs)
			continue
		}

		// Skip if label doesn't match labelWhiteList
		if !labelWhiteList.MatchString(nameForWhiteListing) {
			stderrLogger.Printf("%q does not match the whitelist (%s) and will not be published.", nameForWhiteListing, labelWhiteList.String())
			continue
		}

		labels[label] = value
	}
	return labels, nil
}

// advertiseFeatureLabels advertises the feature labels to a Kubernetes node
// via the NFD server.
func advertiseFeatureLabels(client pb.LabelerClient, labels Labels) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stdoutLogger.Printf("Sending labeling request to nfd-master")

	labelReq := pb.SetLabelsRequest{Labels: labels,
		NfdVersion: version.Get(),
		NodeName:   nodeName}
	_, err := client.SetLabels(ctx, &labelReq)
	if err != nil {
		stderrLogger.Printf("failed to set node labels: %v", err)
		return err
	}

	return nil
}

// UnmarshalJSON implements the Unmarshaler interface from "encoding/json"
func (c *sourcesConfig) UnmarshalJSON(data []byte) error {
	// First do a raw parse to get the per-source data
	raw := map[string]json.RawMessage{}
	err := yaml.Unmarshal(data, &raw)
	if err != nil {
		return err
	}

	// Then parse each source-specific data structure
	// NOTE: we expect 'c' to be pre-populated with correct per-source data
	//       types. Non-pre-populated keys are ignored.
	for k, rawv := range raw {
		if v, ok := (*c)[k]; ok {
			err := yaml.Unmarshal(rawv, &v)
			if err != nil {
				return fmt.Errorf("failed to parse %q source config: %v", k, err)
			}
		}
	}

	return nil
}
