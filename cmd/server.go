// Copyright © 2018 bzon <bryansazon@hotmail.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"

	"github.com/bzon/prometheus-msteams/alert"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v2"
)

// serverCmd represents the server command
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Runs the prometheus-msteams server.",
	Long: `
By using a --config file, you will be able to define multiple prometheus request uri and webhook for different channels.

This is an example config file content in YAML format.

---
connectors:
- channel_1: https://outlook.office.com/webhook/xxxx/hook/for/channel1
- channel_2: https://outlook.office.com/webhook/xxxx/hook/for/channel2

`,
	Run: server,
}

var (
	serverPort          int
	serverListenAddress string
	teamsWebhookURL     string
	requestURI          string
	logLevel            string
	configFile          string
	templateFile        string
	markdownEnabled     bool
)

// TeamsConfig is the struct for config files
// The Connectors key is the request path for Prometheus to post
// The Connectors value is the Teams webhook url
type TeamsConfig struct {
	Connectors []map[string]string `yaml:"connectors"`
}

func init() {
	log.SetFormatter(&log.TextFormatter{})
	RootCmd.AddCommand(serverCmd)
	serverCmd.Flags().IntVarP(&serverPort, "port", "p", 2000,
		"The port on which the server will listen to.")
	serverCmd.Flags().StringVarP(&serverListenAddress, "listen-address", "l",
		"0.0.0.0", "The address on which the server will listen to.")
	serverCmd.Flags().StringVarP(&requestURI, "request-uri", "r", "alertmanager",
		"The default request uri path where Prometheus will post to.")
	serverCmd.Flags().StringVarP(&teamsWebhookURL, "webhook-url", "w", "",
		"The default Microsoft Teams Webhook connector.")
	serverCmd.Flags().StringVar(&logLevel, "log-level", "DEBUG",
		"Log levels: INFO | DEBUG | WARN | ERROR | FATAL | PANIC")
	serverCmd.Flags().BoolVar(&markdownEnabled, "markdown", true,
		"Format the prometheus alert in Microsoft Teams with markdown.")
	serverCmd.Flags().StringVarP(&templateFile, "template-file", "t", "./default-message-card.tmpl",
		"The Microsoft Teams Message Card template file.")
	serverCmd.Flags().StringVar(&configFile, "config", "",
		"The connectors configuration file. "+
			"\nWARNING: 'request-uri' and 'webhook-url' flags will be ignored if this is used.")

	// NOTE: Can we use viper for this?
	// This is placed to support people who still depends
	// on these environment variable as of version 0.0.3
	if v, ok := os.LookupEnv("TEAMS_REQUEST_URI"); ok {
		requestURI = v
	}
	if v, ok := os.LookupEnv("TEAMS_INCOMING_WEBHOOK_URL"); ok {
		teamsWebhookURL = v
	}
	if v, ok := os.LookupEnv("CONFIG_FILE"); ok {
		configFile = v
	}
	if v, ok := os.LookupEnv("TEMPLATE_FILE"); ok {
		templateFile = v
	}
}

func setLogLevel(l string) {
	switch l {
	case "INFO":
		log.SetLevel(log.InfoLevel)
	case "DEBUG":
		log.SetLevel(log.DebugLevel)
	case "WARN":
		log.SetLevel(log.WarnLevel)
	case "ERROR":
		log.SetLevel(log.ErrorLevel)
	case "FATAL":
		log.SetLevel(log.FatalLevel)
	case "PANIC":
		log.SetLevel(log.PanicLevel)
	default:
		log.Fatal("Error: Invalid log-level")
	}
}

func parseConfigFile(f string) *TeamsConfig {
	log.Infof("Parsing the configuration file: %s", configFile)
	b, err := ioutil.ReadFile(f)
	if err != nil {
		log.Fatal(err)
	}
	cfg := &TeamsConfig{}
	if err = yaml.Unmarshal(b, cfg); err != nil {
		log.Fatal(err)
	}
	return cfg
}

func server(cmd *cobra.Command, args []string) {
	setLogLevel(logLevel)
	log.Infof(getVersion())

	funcs := template.DefaultFuncs
	funcs["counter"] = func() func() int {
		i := -1
		return func() int {
			i++
			return i
		}
	}
	template.DefaultFuncs = funcs

	log.Infof("Parsing the message card template file: %s", templateFile)
	if _, err := os.Stat(templateFile); os.IsNotExist(err) {
		log.Errorf("Template File %v does not exist", templateFile)
		os.Exit(1)
	}
	tmpl, err := template.FromGlobs(templateFile)
	if err != nil {
		log.Errorf("Failed to parse template: %v", err)
		os.Exit(1)
	}

	teamsCfg := &TeamsConfig{}
	if configFile != "" {
		log.Warn("If the 'config' flag is used, the" +
			" 'webhook-url' and 'request-uri' flags will be ignored.")
		teamsCfg = parseConfigFile(configFile)
	}

	// If no connectors are found, use the request-uri and webhook-url from flags
	if len(teamsCfg.Connectors) == 0 {
		if requestURI == "" || teamsWebhookURL == "" {
			log.Error("No valid connector configuration found")
			cmd.Usage()
			os.Exit(1)
		}
		cfgFromFlags := map[string]string{requestURI: teamsWebhookURL}
		teamsCfg.Connectors = append(teamsCfg.Connectors, cfgFromFlags)
	}

	mux := http.NewServeMux()
	for _, teamMap := range teamsCfg.Connectors {
		for uri, webhook := range teamMap {
			addPrometheusHandler(uri, webhook, tmpl, mux)
		}
	}
	mux.HandleFunc("/config", teamsCfg.configHandler)
	mux.Handle("/metrics", promhttp.Handler())
	server := serverListenAddress + ":" + strconv.Itoa(serverPort)
	log.Infof("prometheus-msteams server started listening at %s", server)
	log.Fatal(http.ListenAndServe(server, mux))
}

func addPrometheusHandler(uri string, webhook string, template *template.Template, mux *http.ServeMux) {
	promWebhook := alert.PrometheusWebhook{
		RequestURI:      "/" + uri,
		TeamsWebhookURL: webhook,
		MarkdownEnabled: markdownEnabled,
		Template:        template,
	}
	log.Infof("Creating the server request path %q with webhook %q",
		promWebhook.RequestURI, promWebhook.TeamsWebhookURL)
	mux.HandleFunc(promWebhook.RequestURI,
		promWebhook.PrometheusAlertManagerHandler)
}

// configHandler exposes the /config endpoint
func (teamsCfg *TeamsConfig) configHandler(w http.ResponseWriter, r *http.Request) {
	b, err := json.MarshalIndent(teamsCfg.Connectors, "", "  ")
	if err != nil {
		log.Errorf("Failed unmarshalling Teams Connectors config: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, string(b))
}
