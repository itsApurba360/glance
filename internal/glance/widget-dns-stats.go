package glance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var dnsStatsWidgetTemplate = mustParseTemplate("dns-stats.html", "widget-base.html")

type dnsStatsWidget struct {
	widgetBase `yaml:",inline"`

	TimeLabels [8]string `yaml:"-"`
	Stats      *dnsStats `yaml:"-"`

	HourFormat     string `yaml:"hour-format"`
	HideGraph      bool   `yaml:"hide-graph"`
	HideTopDomains bool   `yaml:"hide-top-domains"`
	Service        string `yaml:"service"`
	AllowInsecure  bool   `yaml:"allow-insecure"`
	URL            string `yaml:"url"`
	Token          string `yaml:"token"`
	AppPassword    string `yaml:"app-password"`
	PiHoleVersion  string `yaml:"pihole-version"`
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
}

func makeDNSWidgetTimeLabels(format string) [8]string {
	now := time.Now()
	var labels [8]string

	for h := 24; h > 0; h -= 3 {
		labels[7-(h/3-1)] = strings.ToLower(now.Add(-time.Duration(h) * time.Hour).Format(format))
	}

	return labels
}

func (widget *dnsStatsWidget) initialize() error {
	widget.
		withTitle("DNS Stats").
		withTitleURL(string(widget.URL)).
		withCacheDuration(10 * time.Minute)

	if widget.Service != "adguard" && widget.Service != "pihole" {
		return errors.New("service must be either 'adguard' or 'pihole'")
	}

	return nil
}

func (widget *dnsStatsWidget) update(ctx context.Context) {
	var stats *dnsStats
	var err error

	if widget.Service == "adguard" {
		stats, err = fetchAdguardStats(widget.URL, widget.AllowInsecure, widget.Username, widget.Password, widget.HideGraph)
	} else {
		stats, err = fetchPiholeStats(widget.URL, widget.AllowInsecure, widget.Token, widget.HideGraph, widget.PiHoleVersion, widget.AppPassword)
	}

	if !widget.canContinueUpdateAfterHandlingErr(err) {
		return
	}

	if widget.HourFormat == "24h" {
		widget.TimeLabels = makeDNSWidgetTimeLabels("15:00")
	} else {
		widget.TimeLabels = makeDNSWidgetTimeLabels("3PM")
	}

	widget.Stats = stats
}

func (widget *dnsStatsWidget) Render() template.HTML {
	return widget.renderTemplate(widget, dnsStatsWidgetTemplate)
}

type dnsStats struct {
	TotalQueries      int
	BlockedQueries    int
	BlockedPercent    int
	ResponseTime      int
	DomainsBlocked    int
	Series            [8]dnsStatsSeries
	TopBlockedDomains []dnsStatsBlockedDomain
}

type dnsStatsSeries struct {
	Queries        int
	Blocked        int
	PercentTotal   int
	PercentBlocked int
}

type dnsStatsBlockedDomain struct {
	Domain         string
	PercentBlocked int
}

type adguardStatsResponse struct {
	TotalQueries      int              `json:"num_dns_queries"`
	QueriesSeries     []int            `json:"dns_queries"`
	BlockedQueries    int              `json:"num_blocked_filtering"`
	BlockedSeries     []int            `json:"blocked_filtering"`
	ResponseTime      float64          `json:"avg_processing_time"`
	TopBlockedDomains []map[string]int `json:"top_blocked_domains"`
}

func fetchAdguardStats(instanceURL string, allowInsecure bool, username, password string, noGraph bool) (*dnsStats, error) {
	requestURL := strings.TrimRight(instanceURL, "/") + "/control/stats"

	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}

	request.SetBasicAuth(username, password)

	var client requestDoer
	if !allowInsecure {
		client = defaultHTTPClient
	} else {
		client = defaultInsecureHTTPClient
	}

	responseJson, err := decodeJsonFromRequest[adguardStatsResponse](client, request)
	if err != nil {
		return nil, err
	}

	var topBlockedDomainsCount = min(len(responseJson.TopBlockedDomains), 5)

	stats := &dnsStats{
		TotalQueries:      responseJson.TotalQueries,
		BlockedQueries:    responseJson.BlockedQueries,
		ResponseTime:      int(responseJson.ResponseTime * 1000),
		TopBlockedDomains: make([]dnsStatsBlockedDomain, 0, topBlockedDomainsCount),
	}

	if stats.TotalQueries <= 0 {
		return stats, nil
	}

	stats.BlockedPercent = int(float64(responseJson.BlockedQueries) / float64(responseJson.TotalQueries) * 100)

	for i := 0; i < topBlockedDomainsCount; i++ {
		domain := responseJson.TopBlockedDomains[i]
		var firstDomain string

		for k := range domain {
			firstDomain = k
			break
		}

		if firstDomain == "" {
			continue
		}

		stats.TopBlockedDomains = append(stats.TopBlockedDomains, dnsStatsBlockedDomain{
			Domain: firstDomain,
		})

		if stats.BlockedQueries > 0 {
			stats.TopBlockedDomains[i].PercentBlocked = int(float64(domain[firstDomain]) / float64(responseJson.BlockedQueries) * 100)
		}
	}

	if noGraph {
		return stats, nil
	}

	queriesSeries := responseJson.QueriesSeries
	blockedSeries := responseJson.BlockedSeries

	const bars = 8
	const hoursSpan = 24
	const hoursPerBar int = hoursSpan / bars

	if len(queriesSeries) > hoursSpan {
		queriesSeries = queriesSeries[len(queriesSeries)-hoursSpan:]
	} else if len(queriesSeries) < hoursSpan {
		queriesSeries = append(make([]int, hoursSpan-len(queriesSeries)), queriesSeries...)
	}

	if len(blockedSeries) > hoursSpan {
		blockedSeries = blockedSeries[len(blockedSeries)-hoursSpan:]
	} else if len(blockedSeries) < hoursSpan {
		blockedSeries = append(make([]int, hoursSpan-len(blockedSeries)), blockedSeries...)
	}

	maxQueriesInSeries := 0

	for i := 0; i < bars; i++ {
		queries := 0
		blocked := 0

		for j := 0; j < hoursPerBar; j++ {
			queries += queriesSeries[i*hoursPerBar+j]
			blocked += blockedSeries[i*hoursPerBar+j]
		}

		stats.Series[i] = dnsStatsSeries{
			Queries: queries,
			Blocked: blocked,
		}

		if queries > 0 {
			stats.Series[i].PercentBlocked = int(float64(blocked) / float64(queries) * 100)
		}

		if queries > maxQueriesInSeries {
			maxQueriesInSeries = queries
		}
	}

	for i := 0; i < bars; i++ {
		stats.Series[i].PercentTotal = int(float64(stats.Series[i].Queries) / float64(maxQueriesInSeries) * 100)
	}

	return stats, nil
}

// Legacy Pi-hole stats response (before v6)
type legacyPiholeStatsResponse struct {
	TotalQueries      int                     `json:"dns_queries_today"`
	QueriesSeries     piholeQueriesSeries     `json:"domains_over_time"`
	BlockedQueries    int                     `json:"ads_blocked_today"`
	BlockedSeries     map[int64]int           `json:"ads_over_time"`
	BlockedPercentage float64                 `json:"ads_percentage_today"`
	TopBlockedDomains piholeTopBlockedDomains `json:"top_ads"`
	DomainsBlocked    int                     `json:"domains_being_blocked"`
}

// Pi-hole v6+ response format
type piholeStatsResponse struct {
	Queries struct {
		Total          int     `json:"total"`
		Blocked        int     `json:"blocked"`
		PercentBlocked float64 `json:"percent_blocked"`
	} `json:"queries"`
	Gravity struct {
		DomainsBlocked int `json:"domains_being_blocked"`
	} `json:"gravity"`
	//Note we do not need the full structure. We extract the values needed
	//Adding dummy fields to allow easier json parsing.
	QueriesSeries piholeQueriesSeries `json:"domains_over_time"` // Will always be empty
	BlockedSeries map[int64]int       `json:"ads_over_time"`     // Will always be empty.
}

type piholeTopDomainsResponse map[string]int

// If the user has query logging disabled it's possible for domains_over_time to be returned as an
// empty array rather than a map which will prevent unmashalling the rest of the data so we use
// custom unmarshal behavior to fallback to an empty map.
// See https://github.com/glanceapp/glance/issues/289
type piholeQueriesSeries map[int64]int

func (p *piholeQueriesSeries) UnmarshalJSON(data []byte) error {
	temp := make(map[int64]int)

	err := json.Unmarshal(data, &temp)
	if err != nil {
		*p = make(piholeQueriesSeries)
	} else {
		*p = temp
	}

	return nil
}

// If user has some level of privacy enabled on Pihole, `json:"top_ads"` is an empty array
// Use custom unmarshal behavior to avoid not getting the rest of the valid data when unmarshalling
type piholeTopBlockedDomains map[string]int

func (p *piholeTopBlockedDomains) UnmarshalJSON(data []byte) error {
	// NOTE: do not change to piholeTopBlockedDomains type here or it will cause a stack overflow
	// because of the UnmarshalJSON method getting called recursively
	temp := make(map[string]int)

	err := json.Unmarshal(data, &temp)
	if err != nil {
		*p = make(piholeTopBlockedDomains)
	} else {
		*p = temp
	}

	return nil
}

// piholeGetSID retrieves a new SID from Pi-hole using the app password.
func piholeGetSID(instanceURL, appPassword string, allowInsecure bool) (string, error) {
	var client requestDoer
	if !allowInsecure {
		client = defaultHTTPClient
	} else {
		client = defaultInsecureHTTPClient
	}

	requestURL := strings.TrimRight(instanceURL, "/") + "/api/auth"
	requestBody := []byte(`{"password":"` + appPassword + `"}`)

	request, err := http.NewRequest("POST", requestURL, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", errors.New("failed to create authentication request: " + err.Error())
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("failed to send authentication request: " + err.Error())
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", errors.New("authentication failed, received status: " + response.Status)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", errors.New("failed to read authentication response: " + err.Error())
	}

	var jsonResponse struct {
		Session struct {
			SID string `json:"sid"`
		} `json:"session"`
	}

	if err := json.Unmarshal(body, &jsonResponse); err != nil {
		return "", errors.New("failed to parse authentication response: " + err.Error())
	}

	if jsonResponse.Session.SID == "" {
		return "", errors.New("authentication response did not contain a valid SID")
	}

	return jsonResponse.Session.SID, nil
}

// checkPiholeSID checks if the SID is valid by checking HTTP response status code from /api/auth.
func checkPiholeSID(instanceURL string, sid string, allowInsecure bool) error {
	requestURL := strings.TrimRight(instanceURL, "/") + "/api/auth?sid=" + sid

	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return err
	}

	var client requestDoer
	if !allowInsecure {
		client = defaultHTTPClient
	} else {
		client = defaultInsecureHTTPClient
	}

	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return errors.New("SID is invalid, received status: " + response.Status)
	}

	return nil
}

// fetchPiholeTopDomains fetches the top blocked domains for Pi-hole v6+.
func fetchPiholeTopDomains(instanceURL string, sid string, allowInsecure bool) (piholeTopDomainsResponse, error) {
	requestURL := strings.TrimRight(instanceURL, "/") + "/api/stats/top_domains?blocked=true&sid=" + sid

	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}

	var client requestDoer
	if !allowInsecure {
		client = defaultHTTPClient
	} else {
		client = defaultInsecureHTTPClient
	}

	return decodeJsonFromRequest[piholeTopDomainsResponse](client, request)
}

// fetchPiholeSeries fetches the series data for Pi-hole v6+ (QueriesSeries and BlockedSeries).
func fetchPiholeSeries(instanceURL string, sid string, allowInsecure bool) (piholeQueriesSeries, map[int64]int, error) {
	requestURL := strings.TrimRight(instanceURL, "/") + "/api/history?sid=" + sid

	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, nil, err
	}

	var client requestDoer
	if !allowInsecure {
		client = defaultHTTPClient
	} else {
		client = defaultInsecureHTTPClient
	}

	// Define the correct struct to match the API response
	var responseJson struct {
		History []struct {
			Timestamp int64 `json:"timestamp"`
			Total     int   `json:"total"`
			Blocked   int   `json:"blocked"`
		} `json:"history"`
	}

	err = decodeJsonInto(client, request, &responseJson)
	if err != nil {
		return nil, nil, err
	}

	queriesSeries := make(piholeQueriesSeries)
	blockedSeries := make(map[int64]int)

	// Populate the series data from history array
	for _, entry := range responseJson.History {
		queriesSeries[entry.Timestamp] = entry.Total
		blockedSeries[entry.Timestamp] = entry.Blocked
	}

	return queriesSeries, blockedSeries, nil
}

// Helper functions to process the responses
func parsePiholeStats(r *piholeStatsResponse, topDomains piholeTopDomainsResponse) *dnsStats {

	stats := &dnsStats{
		TotalQueries:   r.Queries.Total,
		BlockedQueries: r.Queries.Blocked,
		BlockedPercent: int(r.Queries.PercentBlocked),
		DomainsBlocked: r.Gravity.DomainsBlocked,
	}

	if len(topDomains) > 0 {
		domains := make([]dnsStatsBlockedDomain, 0, len(topDomains))
		for domain, count := range topDomains {
			domains = append(domains, dnsStatsBlockedDomain{
				Domain:         domain,
				PercentBlocked: int(float64(count) / float64(r.Queries.Blocked) * 100), // Calculate percentage here
			})
		}

		sort.Slice(domains, func(a, b int) bool {
			return domains[a].PercentBlocked > domains[b].PercentBlocked
		})
		stats.TopBlockedDomains = domains[:min(len(domains), 5)]
	}

	return stats
}
func parsePiholeStatsLegacy(r *legacyPiholeStatsResponse, noGraph bool) *dnsStats {

	stats := &dnsStats{
		TotalQueries:   r.TotalQueries,
		BlockedQueries: r.BlockedQueries,
		BlockedPercent: int(r.BlockedPercentage),
		DomainsBlocked: r.DomainsBlocked,
	}
	if len(r.TopBlockedDomains) > 0 {
		domains := make([]dnsStatsBlockedDomain, 0, len(r.TopBlockedDomains))

		for domain, count := range r.TopBlockedDomains {
			domains = append(domains, dnsStatsBlockedDomain{
				Domain:         domain,
				PercentBlocked: int(float64(count) / float64(r.BlockedQueries) * 100),
			})
		}

		sort.Slice(domains, func(a, b int) bool {
			return domains[a].PercentBlocked > domains[b].PercentBlocked
		})

		stats.TopBlockedDomains = domains[:min(len(domains), 5)]
	}
	if noGraph {
		return stats
	}

	// Pihole _should_ return data for the last 24 hours in a 10 minute interval, 6*24 = 144
	if len(r.QueriesSeries) != 144 || len(r.BlockedSeries) != 144 {
		return stats
	}

	var lowestTimestamp int64 = 0
	for timestamp := range r.QueriesSeries {
		if lowestTimestamp == 0 || timestamp < lowestTimestamp {
			lowestTimestamp = timestamp
		}
	}
	maxQueriesInSeries := 0

	for i := 0; i < 8; i++ {
		queries := 0
		blocked := 0
		for j := 0; j < 18; j++ {
			index := lowestTimestamp + int64(i*10800+j*600)
			queries += r.QueriesSeries[index]
			blocked += r.BlockedSeries[index]
		}
		if queries > maxQueriesInSeries {
			maxQueriesInSeries = queries
		}
		stats.Series[i] = dnsStatsSeries{
			Queries: queries,
			Blocked: blocked,
		}
		if queries > 0 {
			stats.Series[i].PercentBlocked = int(float64(blocked) / float64(queries) * 100)
		}
	}
	for i := 0; i < 8; i++ {
		stats.Series[i].PercentTotal = int(float64(stats.Series[i].Queries) / float64(maxQueriesInSeries) * 100)
	}
	return stats
}

func fetchPiholeStats(instanceURL string, allowInsecure bool, token string, noGraph bool, version, appPassword string) (*dnsStats, error) {
	instanceURL = strings.TrimRight(instanceURL, "/")
	var requestURL string
	var sid string
	isV6 := version == "" || version == "6"

	if isV6 {
		if appPassword == "" {
			return nil, errors.New("missing app password")
		}

		sid = os.Getenv("SID")
		if sid == "" {
			newSid, err := piholeGetSID(instanceURL, appPassword, allowInsecure)
			if err != nil {
				return nil, fmt.Errorf("failed to get SID: %w", err)
			}
			sid = newSid
			os.Setenv("SID", sid)
		} else {
			err := checkPiholeSID(instanceURL, sid, allowInsecure)
			if err != nil {
				newSid, err := piholeGetSID(instanceURL, appPassword, allowInsecure)
				if err != nil {
					return nil, fmt.Errorf("failed to get SID after invalid check: %w", err)
				}
				sid = newSid
				os.Setenv("SID", sid)
			}
		}

		requestURL = instanceURL + "/api/stats/summary?sid=" + sid
	} else {
		if token == "" {
			return nil, errors.New("missing API token")
		}
		requestURL = instanceURL + "/admin/api.php?summaryRaw&topItems&overTimeData10mins&auth=" + token
	}

	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	var client requestDoer
	if !allowInsecure {
		client = defaultHTTPClient
	} else {
		client = defaultInsecureHTTPClient
	}

	var responseJson interface{}
	if isV6 {
		responseJson, err = decodeJsonFromRequest[piholeStatsResponse](client, request)
	} else {
		responseJson, err = decodeJsonFromRequest[legacyPiholeStatsResponse](client, request)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to decode JSON response: %w", err)
	}

	switch r := responseJson.(type) {
	case *piholeStatsResponse:
		// Fetch top domains separately for v6+
		topDomains, err := fetchPiholeTopDomains(instanceURL, sid, allowInsecure)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch top domains: %w", err)
		}

		// Fetch series data separately for v6+
		queriesSeries, blockedSeries, err := fetchPiholeSeries(instanceURL, sid, allowInsecure)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch queries series: %w", err)
		}

		// Merge series data
		r.QueriesSeries = queriesSeries
		r.BlockedSeries = blockedSeries

		return parsePiholeStats(r, topDomains), nil

	case *legacyPiholeStatsResponse:
		return parsePiholeStatsLegacy(r, noGraph), nil

	default:
		return nil, errors.New("unexpected response type")
	}
}
