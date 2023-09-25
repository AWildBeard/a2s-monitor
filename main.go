package main

import (
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rumblefrog/go-a2s"
	flag "github.com/spf13/pflag"
	"maps"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	infoInterval   time.Duration
	timeout        time.Duration
	playerInterval time.Duration
	listenAddress  string
	hosts          []string
	printVersion   bool
	debug          bool

	buildType    string
	buildVersion string

	// infoLabelKey allows removing metric fields when the label set for infoLabels changes because of a map change,
	// updated server version, etc.
	infoLabelKey = "host_port"

	// playerLabelKey allows removing metric fields when the label set for playerLabels changes because of map change,
	// etc.
	playerLabelKey = "player_name"

	// commonLabels gets appended to infoLabels and playerLabels in init
	commonLabels = []string{infoLabelKey, "map", "version", "steam_appid"}

	// infoLabels are labels attached to server specific metrics
	infoLabels = []string{"server_name"}

	// playerLabels are labels attached to player specific metrics
	playerLabels = []string{playerLabelKey}
)

func getCommonLabels(host string, i *a2s.ServerInfo) prometheus.Labels {
	return map[string]string{
		infoLabelKey:  host,
		"map":         i.Map,
		"version":     i.Version,
		"steam_appid": strconv.Itoa(int(i.ID)),
	}
}

func getInfoLabels(i *a2s.ServerInfo, commonLabels prometheus.Labels) prometheus.Labels {
	labels := map[string]string{
		"server_name": i.Name,
	}

	maps.Copy(labels, commonLabels)

	return labels
}

func getPlayerLabels(p *a2s.Player, commonLabels prometheus.Labels) prometheus.Labels {
	labels := map[string]string{
		"player_name": p.Name,
	}

	maps.Copy(labels, commonLabels)

	return labels
}

func init() {
	infoLabels = append(infoLabels, commonLabels...)
	playerLabels = append(playerLabels, commonLabels...)

	flag.DurationVarP(&infoInterval, "info-interval", "i", 5*time.Minute,
		"the interval to query the target a2s port for game info. This will include map data, number of players "+
			"maximum player slots, game version, etc. This method is also used for latency calculation per valve spec.")
	flag.DurationVarP(&timeout, "timeout", "t", 5*time.Second,
		"timeout for all operations. Default should be fine.")
	flag.DurationVarP(&playerInterval, "player-interval", "p", 5*time.Minute,
		"the interval to query for player information. This will include player names, play time, and \"score\".")
	flag.StringVarP(&listenAddress, "listen", "l", "127.0.0.1:9321",
		"set the listen address for the prometheus metrics server")
	flag.StringSliceVarP(&hosts, "hosts", "h", []string{},
		"[domain name]:[port] or [ip]:[port] pairs for target servers to monitor. This should be the A2S port. "+
			"Sometimes it's one and the same but it can be a different port. For example in ArmA3 it's 2303 where the game "+
			"port is 2302.")
	flag.BoolVarP(&printVersion, "version", "v", false,
		"print version information and exit")
	flag.BoolVarP(&debug, "debug", "d", false,
		"Enable debug mode printing")
}

type infoUpdate struct {
	host         string
	online       bool
	queryLatency time.Duration
	*a2s.ServerInfo
	commonLabels prometheus.Labels
}

func newInfoUpdate(host string, si *a2s.ServerInfo, latency time.Duration) *infoUpdate {
	commonLabels := getCommonLabels(host, si)
	return &infoUpdate{
		host:         host,
		online:       true,
		queryLatency: latency,
		ServerInfo:   si,
		commonLabels: commonLabels,
	}
}

type playerUpdate struct {
	host string
	*a2s.PlayerInfo
	commonLabels prometheus.Labels
}

func newPlayerUpdate(host string, commonLabels prometheus.Labels, p *a2s.PlayerInfo) *playerUpdate {
	return &playerUpdate{
		host:         host,
		PlayerInfo:   p,
		commonLabels: commonLabels,
	}
}

func main() {
	flag.Parse()

	if printVersion {
		fmt.Printf("%s-%s-%s\n", buildVersion, buildType, runtime.Version())
		os.Exit(1)
	}

	if len(hosts) <= 0 {
		fmt.Printf("You must supply at least 1 monitor target\n")
		os.Exit(1)
	}

	providedToResolvedHosts := resolveHosts(hosts)

	playerCount := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "a2s_monitor",
		Name:      "player_count",
		Help:      "current number of players in the server",
	}, infoLabels)

	maxPlayerCount := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "a2s_monitor",
		Name:      "max_player_count",
		Help:      "maximum number of players in the server",
	}, infoLabels)

	botCount := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "a2s_monitor",
		Name:      "bot_count",
		Help:      "current number of bots in the server",
	}, infoLabels)

	applicationLatency := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "a2s_monitor",
		Name:      "observed_app_latency",
		Help:      "the latency observed by sending A2S info requests (as part of spec for PING replacement)",
	}, []string{infoLabelKey})

	online := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "a2s_monitor",
		Name:      "online",
		Help:      "0 or 1 if server is online. 0 == offline, 1 == online",
	}, []string{infoLabelKey})

	playerScore := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "a2s_monitor",
		Name:      "player_score",
		Help:      "current score for a given player",
	}, playerLabels)

	playTime := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "a2s_monitor",
		Name:      "play_time",
		Help:      "session play time for a given player",
	}, playerLabels)

	prometheus.MustRegister(playerCount)
	prometheus.MustRegister(maxPlayerCount)
	prometheus.MustRegister(botCount)
	prometheus.MustRegister(applicationLatency)
	prometheus.MustRegister(online)
	prometheus.MustRegister(playerScore)
	prometheus.MustRegister(playTime)
	clearMetricsWithLabels := func(labelMatch prometheus.Labels) {
		playerScore.DeletePartialMatch(labelMatch)
		playTime.DeletePartialMatch(labelMatch)
		playerCount.DeletePartialMatch(labelMatch)
		maxPlayerCount.DeletePartialMatch(labelMatch)
		botCount.DeletePartialMatch(labelMatch)
	}

	http.Handle("/metrics", promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		},
	))

	infoUpdates := make(chan *infoUpdate, 100)
	playerUpdates := make(chan *playerUpdate, 100)

	go func(infoUpdates chan *infoUpdate, playerUpdates chan *playerUpdate) {
		commonLabelMapping := map[string]prometheus.Labels{}

		playersLastPlayTimeReported := map[string]float32{}
		for {
			select {
			case infoUpdate := <-infoUpdates:

				if !infoUpdate.online {
					hostPortLabelMatch := prometheus.Labels{infoLabelKey: infoUpdate.host}
					clearMetricsWithLabels(hostPortLabelMatch)
					applicationLatency.WithLabelValues(infoUpdate.host).Set(0)
					online.WithLabelValues(infoUpdate.host).Set(0)
					continue
				} else {
					online.WithLabelValues(infoUpdate.host).Set(1)
					applicationLatency.WithLabelValues(infoUpdate.host).Set(float64(infoUpdate.queryLatency.Milliseconds()))
				}

				if labels, ok := commonLabelMapping[infoUpdate.host]; ok {
					// Our common labels have changed so we need to wipe out all our old metrics (including player metrics).
					// When you think about it this makes sense. We don't want to continue to log play time for a map
					// that is no longer running... Or we don't want to log play time when the server has shutdown :eyes:.
					if !maps.Equal(infoUpdate.commonLabels, labels) {
						hostPortLabelMatch := prometheus.Labels{infoLabelKey: infoUpdate.host}

						clearMetricsWithLabels(hostPortLabelMatch)

						commonLabelMapping[infoUpdate.host] = infoUpdate.commonLabels
					}

					// Don't update the labels here because they are the same. Lol just saving the 5 memory writes :rofl:
				} else {
					commonLabelMapping[infoUpdate.host] = infoUpdate.commonLabels
				}
				// At this point commonLabelMapping[infoUpdate.host] == infoUpdate.commonLabels, so we'll just use those.
				infoLabels := getInfoLabels(infoUpdate.ServerInfo, infoUpdate.commonLabels)

				playerCount.With(infoLabels).Set(float64(infoUpdate.Players))
				maxPlayerCount.With(infoLabels).Set(float64(infoUpdate.MaxPlayers))
				botCount.With(infoLabels).Set(float64(infoUpdate.Bots))

			case playerUpdate := <-playerUpdates:
				// Clear out our player playtime difference cache of players that don't exist anymore
				if len(playerUpdate.Players) > 0 {
					newLastPlaytimeCache := map[string]float32{}

					if len(playersLastPlayTimeReported) > 0 {
						for _, player := range playerUpdate.Players {
							playerTrackingString := playerUpdate.commonLabels["host_port"] + player.Name + playerUpdate.commonLabels["map"] + playerUpdate.commonLabels["version"]
							if t, ok := playersLastPlayTimeReported[playerTrackingString]; ok {
								newLastPlaytimeCache[playerTrackingString] = t
							}
						}

						playersLastPlayTimeReported = newLastPlaytimeCache
					}
				} else {
					// clear the cache in the event no players are reported on the server.
					if len(playersLastPlayTimeReported) > 0 {
						playersLastPlayTimeReported = map[string]float32{}
					}
				}

				// So we don't get forward observability into metrics afaict... So we must clear out the host player
				// metric prior to writing so we don't get phantom players being recorded when they are no longer
				// present according to our information.
				hostPortLabelMatch := prometheus.Labels{infoLabelKey: playerUpdate.host}

				playerScore.DeletePartialMatch(hostPortLabelMatch)
				// Now populate the metric again...
				for _, player := range playerUpdate.Players {
					playerScore.With(getPlayerLabels(player, playerUpdate.commonLabels)).Set(float64(player.Score))
				}

				playTime.DeletePartialMatch(hostPortLabelMatch)
				// Now populate the metric again...
				for _, player := range playerUpdate.Players {
					playerTrackingString := playerUpdate.commonLabels["host_port"] + player.Name + playerUpdate.commonLabels["map"] + playerUpdate.commonLabels["version"]

					timeToReport := player.Duration
					if t, ok := playersLastPlayTimeReported[playerTrackingString]; ok {
						playersLastPlayTimeReported[playerTrackingString] = timeToReport
						timeToReport = timeToReport - t
					} else {
						playersLastPlayTimeReported[playerTrackingString] = timeToReport
					}

					playTime.With(getPlayerLabels(player, playerUpdate.commonLabels)).Set(float64(timeToReport))
				}
			}
		}

	}(infoUpdates, playerUpdates)

	for providedHostPort, resolvedHostPort := range providedToResolvedHosts {
		go func(providedHostPort, resolvedHostPort string, infoUpdateChan chan *infoUpdate, playerUpdateChan chan *playerUpdate) {
			infoTicker := time.NewTicker(infoInterval)
			playerTicker := time.NewTicker(playerInterval)

			hostCommonLabels := doInfoUpdate(providedHostPort, resolvedHostPort, infoUpdateChan)
			if hostCommonLabels != nil && len(hostCommonLabels) > 0 {
				doPlayerUpdate(providedHostPort, resolvedHostPort, hostCommonLabels, playerUpdateChan)
			}

			for {
				select {
				case <-infoTicker.C:
					hostCommonLabels = doInfoUpdate(providedHostPort, resolvedHostPort, infoUpdateChan)
					infoTicker.Reset(infoInterval)
				case <-playerTicker.C:
					// Don't attempt playerUpdates when the host isn't online
					if hostCommonLabels == nil || len(hostCommonLabels) <= 0 {
						playerTicker.Reset(playerInterval)
						continue
					}

					doPlayerUpdate(providedHostPort, resolvedHostPort, hostCommonLabels, playerUpdateChan)
					playerTicker.Reset(playerInterval)
				}
			}
		}(providedHostPort, resolvedHostPort, infoUpdates, playerUpdates)
	}

	fmt.Println("starting http server")
	fmt.Printf("%v\n", http.ListenAndServe(listenAddress, nil))
	os.Exit(1)
}

func resolveHosts(hosts []string) map[string]string {
	resolvedHosts := map[string]string{}

	for _, hostPort := range hosts {
		if host, port, ok := strings.Cut(hostPort, ":"); ok {
			var ip net.IP
			if ip = net.ParseIP(host); ip == nil {
				ctxt, cncl := context.WithTimeout(context.Background(), timeout)
				if ips, err := net.DefaultResolver.LookupIP(ctxt, "ip", host); err == nil {
					cncl()
					ip = ips[0]
				} else {
					cncl()
					fmt.Printf("%v doesn't appear to be a IP and it failed to resolve: %v\n", host, err)
					os.Exit(1)
				}
			}

			resolvedHosts[hostPort] = ip.String() + ":" + port
		} else {
			fmt.Printf("%v does not appear to be in the [host]:[port] format\n")
			os.Exit(1)
		}
	}

	return resolvedHosts
}

func doInfoUpdate(providedHostPort, resolvedHostPort string, infoUpdateChan chan *infoUpdate) prometheus.Labels {
	client, err := a2s.NewClient(resolvedHostPort, a2s.SetMaxPacketSize(64000), a2s.TimeoutOption(timeout))
	if err != nil {
		fmt.Printf("failed to create client to host (is the host down?) %v: %v\n", resolvedHostPort, err)
		infoUpdateChan <- &infoUpdate{online: false}
		return nil
	}

	defer func() {
		err := client.Close()
		if err != nil {
			fmt.Printf("failed to close client connection to %v: %v\n", resolvedHostPort, err)
		}
	}()

	start := time.Now()
	info, err := client.QueryInfo()
	latency := time.Now().Sub(start)
	if err != nil {
		if debug {
			// servers being down are common. Don't say it like it's an error.
			// Any of these other errors are likely more noteworthy.
			fmt.Printf("failed to get info from host %v: %v\n", resolvedHostPort, err)
		}
		infoUpdateChan <- &infoUpdate{online: false}
		return nil
	}

	update := newInfoUpdate(providedHostPort, info, latency)
	infoUpdateChan <- update
	return update.commonLabels
}

func doPlayerUpdate(providedHostPort, resolvedHostPort string, commonLabels prometheus.Labels, playerUpdateChan chan *playerUpdate) {
	client, err := a2s.NewClient(resolvedHostPort, a2s.SetMaxPacketSize(64000), a2s.TimeoutOption(timeout))
	if err != nil {
		fmt.Printf("failed to create client to host (is the host down?) %v: %v\n", resolvedHostPort, err)
		return
	}

	defer func() {
		err := client.Close()
		if err != nil {
			fmt.Printf("failed to close client connection to %v: %v\n", resolvedHostPort, err)
		}
	}()

	players, err := client.QueryPlayer()
	if err != nil {
		fmt.Printf("failed to get players from host %v: %v\n", resolvedHostPort, err)
		return
	}

	playerUpdateChan <- newPlayerUpdate(providedHostPort, commonLabels, players)
}
