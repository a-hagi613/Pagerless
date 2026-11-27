package main

import (
	"encoding/json"
	"net/http"
)

// The integration fixture serves the same alert and dashboard shapes as the
// observability stack in pagerless-sandbox. It is not a Prometheus or Grafana
// implementation; it gives the MCP providers deterministic data to read.

const prometheusAlerts = `{"status":"success","data":{"alerts":[
 {"labels":{"alertname":"ContainerOOMKilled","namespace":"shop","deployment":"checkout","severity":"warning"},
  "annotations":{"summary":"A container of a Target was OOMKilled."},
  "state":"firing","activeAt":"2026-11-27T10:00:00Z","value":"1"},
 {"labels":{"alertname":"ServiceErrorRateHigh","namespace":"shop","deployment":"checkout","severity":"warning"},
  "annotations":{"summary":"A Target's server spans are failing."},
  "state":"firing","activeAt":"2026-11-27T10:01:00Z","value":"0.08"},
 {"labels":{"alertname":"ContainerMemoryGrowth","namespace":"shop","deployment":"checkout","severity":"warning"},
  "annotations":{"summary":"A container's working set is growing."},
  "state":"pending","activeAt":"2026-11-27T10:02:00Z","value":"0.2"}
]}}`

const prometheusQuery = `{"status":"success","data":{"resultType":"vector","result":[
 {"metric":{"__name__":"container_memory_working_set_bytes","namespace":"shop",
  "pod":"checkout-7d8f9c6b5-abcde","container":"checkout"},
  "value":[1795758000,"134217728"]}
]}}`

const prometheusTargets = `{"status":"success","data":{"activeTargets":[
 {"labels":{"job":"kube-state-metrics","namespace":"monitoring"},
  "scrapeUrl":"http://kube-state-metrics:8080/metrics","health":"up","lastScrape":"2026-11-27T10:03:00Z"},
 {"labels":{"job":"pagerless-server","namespace":"pagerless"},
  "scrapeUrl":"http://pagerless-server:9090/metrics","health":"up","lastScrape":"2026-11-27T10:03:00Z"}
]}}`

const grafanaAlerts = `[
 {"labels":{"alertname":"ContainerOOMKilled","namespace":"shop","deployment":"checkout","severity":"warning"},
  "annotations":{"summary":"A container of a Target was OOMKilled."},
  "state":"active","startsAt":"2026-11-27T10:00:00Z"},
 {"labels":{"alertname":"ServiceErrorRateHigh","namespace":"shop","deployment":"checkout","severity":"warning"},
  "annotations":{"summary":"A Target's server spans are failing."},
  "state":"active","startsAt":"2026-11-27T10:01:00Z"}
]`

const grafanaSearch = `[
 {"uid":"pagerless-target","title":"Pagerless Target metrics","type":"dash-db","tags":["pagerless","target"]},
 {"uid":"pagerless-target-logs","title":"Pagerless Target logs","type":"dash-db","tags":["pagerless","logs"]}
]`

const grafanaDashboard = `{"dashboard":{
 "uid":"pagerless-target","title":"Pagerless Target metrics","tags":["pagerless","target"],
 "templating":{"list":[{"name":"namespace"},{"name":"target"}]},
 "panels":[
  {"title":"CPU","type":"timeseries"},
  {"title":"Memory","type":"timeseries"},
  {"title":"Restarts","type":"stat"}
 ]
},"meta":{"slug":"pagerless-target"}}`

const grafanaDatasources = `[
 {"name":"Prometheus","type":"prometheus","url":"http://prometheus:9090","isDefault":true,
  "secureJsonData":{"password":"must-not-leak"}},
 {"name":"Loki","type":"loki","url":"http://loki:3100",
  "secureJsonData":{"basicAuthPassword":"must-not-leak"}}
]`

// prometheusHandler serves the read-only Prometheus API subset the provider uses.
func prometheusHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/alerts", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, json.RawMessage(prometheusAlerts))
	})
	mux.HandleFunc("GET /api/v1/query", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, json.RawMessage(prometheusQuery))
	})
	mux.HandleFunc("GET /api/v1/targets", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, json.RawMessage(prometheusTargets))
	})
	return mux
}

// grafanaHandler serves the read-only Grafana API subset the provider uses.
func grafanaHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/alertmanager/grafana/api/v2/alerts",
		func(response http.ResponseWriter, _ *http.Request) {
			writeJSON(response, http.StatusOK, json.RawMessage(grafanaAlerts))
		})
	mux.HandleFunc("GET /api/search", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, json.RawMessage(grafanaSearch))
	})
	mux.HandleFunc("GET /api/dashboards/uid/pagerless-target",
		func(response http.ResponseWriter, _ *http.Request) {
			writeJSON(response, http.StatusOK, json.RawMessage(grafanaDashboard))
		})
	mux.HandleFunc("GET /api/datasources", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, json.RawMessage(grafanaDatasources))
	})
	return mux
}
