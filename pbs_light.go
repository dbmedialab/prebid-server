package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"sort"
	"strconv"
	"time"

	"github.com/cloudfoundry/gosigar"
	"github.com/golang/glog"
	"github.com/julienschmidt/httprouter"
	"github.com/rs/cors"
	"github.com/spf13/viper"
	"github.com/xeipuuv/gojsonschema"
	"xojoc.pw/useragent"

	"os"
	"os/signal"
	"syscall"

	"github.com/dbmedialab/prebid-server/adapters"
	"github.com/dbmedialab/prebid-server/cache"
	"github.com/dbmedialab/prebid-server/cache/dummycache"
	"github.com/dbmedialab/prebid-server/cache/filecache"
	"github.com/dbmedialab/prebid-server/cache/postgrescache"
	"github.com/dbmedialab/prebid-server/config"
	"github.com/dbmedialab/prebid-server/pbs"
	"github.com/dbmedialab/prebid-server/pbsmetrics"
	"github.com/dbmedialab/prebid-server/prebid"
	pbc "github.com/dbmedialab/prebid-server/prebid_cache_client"
)

var hostCookieSettings pbs.HostCookieSettings

var exchanges map[string]adapters.Adapter
var dataCache cache.Cache
var reqSchema *gojsonschema.Schema

type bidResult struct {
	bidder   *pbs.PBSBidder
	bid_list pbs.PBSBidSlice
}

const schemaDirectory = "./static/bidder-params"

const defaultPriceGranularity = "med"

// Constant keys for ad server targeting for responses to Prebid Mobile
const hbpbConstantKey = "hb_pb"
const hbCreativeLoadMethodConstantKey = "hb_creative_loadtype"
const hbBidderConstantKey = "hb_bidder"
const hbCacheIdConstantKey = "hb_cache_id"
const hbSizeConstantKey = "hb_size"

// hb_creative_loadtype key can be one of `demand_sdk` or `html`
// default is `html` where the creative is loaded in the primary ad server's webview through AppNexus hosted JS
// `demand_sdk` is for bidders who insist on their creatives being loaded in their own SDK's webview
const hbCreativeLoadMethodHTML = "html"
const hbCreativeLoadMethodDemandSDK = "demand_sdk"

func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

func writeAuctionError(w http.ResponseWriter, s string, err error) {
	var resp pbs.PBSResponse
	if err != nil {
		resp.Status = fmt.Sprintf("%s: %v", s, err)
	} else {
		resp.Status = s
	}
	b, err := json.Marshal(&resp)
	if err != nil {
		glog.Errorf("Failed to marshal auction error JSON: %s", err)
	} else {
		w.Write(b)
	}
}

type cookieSyncRequest struct {
	UUID    string   `json:"uuid"`
	Bidders []string `json:"bidders"`
}

type cookieSyncResponse struct {
	UUID         string           `json:"uuid"`
	Status       string           `json:"status"`
	BidderStatus []*pbs.PBSBidder `json:"bidder_status"`
}

type cookieSyncDeps struct {
	m *pbsmetrics.Metrics
}

func (deps *cookieSyncDeps) cookieSync(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	deps.m.CookieSyncMeter.Mark(1)
	userSyncCookie := pbs.ParsePBSCookieFromRequest(r)
	if !userSyncCookie.AllowSyncs() {
		http.Error(w, "User has opted out", http.StatusUnauthorized)
		return
	}

	defer r.Body.Close()

	csReq := &cookieSyncRequest{}
	err := json.NewDecoder(r.Body).Decode(&csReq)
	if err != nil {
		if glog.V(2) {
			glog.Infof("Failed to parse /cookie_sync request body: %v", err)
		}
		http.Error(w, "JSON parse failed", http.StatusBadRequest)
		return
	}

	csResp := cookieSyncResponse{
		UUID:         csReq.UUID,
		BidderStatus: make([]*pbs.PBSBidder, 0, len(csReq.Bidders)),
	}

	if userSyncCookie.LiveSyncCount() == 0 {
		csResp.Status = "no_cookie"
	} else {
		csResp.Status = "ok"
	}

	for _, bidder := range csReq.Bidders {
		if ex, ok := exchanges[bidder]; ok {
			if !userSyncCookie.HasLiveSync(ex.FamilyName()) {
				b := pbs.PBSBidder{
					BidderCode:   bidder,
					NoCookie:     true,
					UsersyncInfo: ex.GetUsersyncInfo(),
				}
				csResp.BidderStatus = append(csResp.BidderStatus, &b)
			}
		}
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	//enc.SetIndent("", "  ")
	enc.Encode(csResp)
}

type auctionDeps struct {
	m *pbsmetrics.Metrics
}

func (deps *auctionDeps) auction(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	w.Header().Add("Content-Type", "application/json")

	deps.m.RequestMeter.Mark(1)

	isSafari := false
	if ua := useragent.Parse(r.Header.Get("User-Agent")); ua != nil {
		if ua.Type == useragent.Browser && ua.Name == "Safari" {
			isSafari = true
			deps.m.SafariRequestMeter.Mark(1)
		}
	}

	pbs_req, err := pbs.ParsePBSRequest(r, dataCache, &hostCookieSettings)
	if err != nil {
		if glog.V(2) {
			glog.Infof("Failed to parse /auction request: %v", err)
		}
		writeAuctionError(w, "Error parsing request", err)
		deps.m.ErrorMeter.Mark(1)
		return
	}

	status := "OK"
	if pbs_req.App != nil {
		deps.m.AppRequestMeter.Mark(1)
	} else if pbs_req.Cookie.LiveSyncCount() == 0 {
		deps.m.NoCookieMeter.Mark(1)
		if isSafari {
			deps.m.SafariNoCookieMeter.Mark(1)
		}
		status = "no_cookie"
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*time.Duration(pbs_req.TimeoutMillis))
	defer cancel()

	account, err := dataCache.Accounts().Get(pbs_req.AccountID)
	if err != nil {
		if glog.V(2) {
			glog.Infof("Invalid account id: %v", err)
		}
		writeAuctionError(w, "Unknown account id", fmt.Errorf("Unknown account"))
		deps.m.ErrorMeter.Mark(1)
		return
	}

	am := deps.m.GetAccountMetrics(pbs_req.AccountID)
	am.RequestMeter.Mark(1)

	pbs_resp := pbs.PBSResponse{
		Status:       status,
		TID:          pbs_req.Tid,
		BidderStatus: pbs_req.Bidders,
	}

	ch := make(chan bidResult)
	sentBids := 0
	for _, bidder := range pbs_req.Bidders {
		if ex, ok := exchanges[bidder.BidderCode]; ok {
			ametrics := deps.m.AdapterMetrics[bidder.BidderCode]
			accountAdapterMetric := am.AdapterMetrics[bidder.BidderCode]
			ametrics.RequestMeter.Mark(1)
			accountAdapterMetric.RequestMeter.Mark(1)
			if pbs_req.App == nil {
				uid, _, _ := pbs_req.Cookie.GetUID(ex.FamilyName())
				if uid == "" {
					bidder.NoCookie = true
					bidder.UsersyncInfo = ex.GetUsersyncInfo()
					ametrics.NoCookieMeter.Mark(1)
					accountAdapterMetric.NoCookieMeter.Mark(1)
					if ex.SkipNoCookies() {
						continue
					}
				}
			}
			sentBids++
			go func(bidder *pbs.PBSBidder) {
				start := time.Now()
				bid_list, err := ex.Call(ctx, pbs_req, bidder)
				bidder.ResponseTime = int(time.Since(start) / time.Millisecond)
				ametrics.RequestTimer.UpdateSince(start)
				accountAdapterMetric.RequestTimer.UpdateSince(start)
				if err != nil {
					switch err {
					case context.DeadlineExceeded:
						ametrics.TimeoutMeter.Mark(1)
						accountAdapterMetric.TimeoutMeter.Mark(1)
						bidder.Error = "Timed out"
					case context.Canceled:
						fallthrough
					default:
						ametrics.ErrorMeter.Mark(1)
						accountAdapterMetric.ErrorMeter.Mark(1)
						bidder.Error = err.Error()
						glog.Warningf("Error from bidder %v. Ignoring all bids: %v", bidder.BidderCode, err)
					}
				} else if bid_list != nil {
					bid_list = checkForValidBidSize(bid_list, bidder)
					bidder.NumBids = len(bid_list)
					am.BidsReceivedMeter.Mark(int64(bidder.NumBids))
					accountAdapterMetric.BidsReceivedMeter.Mark(int64(bidder.NumBids))
					for _, bid := range bid_list {
						var cpm = int64(bid.Price * 1000)
						ametrics.PriceHistogram.Update(cpm)
						am.PriceHistogram.Update(cpm)
						accountAdapterMetric.PriceHistogram.Update(cpm)
						bid.ResponseTime = bidder.ResponseTime
					}
				} else {
					bidder.NoBid = true
					ametrics.NoBidMeter.Mark(1)
					accountAdapterMetric.NoBidMeter.Mark(1)
				}

				ch <- bidResult{
					bidder:   bidder,
					bid_list: bid_list,
				}
			}(bidder)

		} else {
			bidder.Error = "Unsupported bidder"
		}
	}

	for i := 0; i < sentBids; i++ {
		result := <-ch

		for _, bid := range result.bid_list {
			pbs_resp.Bids = append(pbs_resp.Bids, bid)
		}
	}
	if pbs_req.CacheMarkup == 1 {
		cobjs := make([]*pbc.CacheObject, len(pbs_resp.Bids))
		for i, bid := range pbs_resp.Bids {
			bc := &pbc.BidCache{
				Adm:    bid.Adm,
				NURL:   bid.NURL,
				Width:  bid.Width,
				Height: bid.Height,
			}
			cobjs[i] = &pbc.CacheObject{
				Value: bc,
			}
		}
		err = pbc.Put(ctx, cobjs)
		if err != nil {
			writeAuctionError(w, "Prebid cache failed", err)
			deps.m.ErrorMeter.Mark(1)
			return
		}
		for i, bid := range pbs_resp.Bids {
			bid.CacheID = cobjs[i].UUID
			bid.NURL = ""
			bid.Adm = ""
		}
	}

	if pbs_req.SortBids == 1 {
		sortBidsAddKeywordsMobile(pbs_resp.Bids, pbs_req, account.PriceGranularity)
	}

	if glog.V(2) {
		glog.Infof("Request for %d ad units on url %s by account %s got %d bids", len(pbs_req.AdUnits), pbs_req.Url, pbs_req.AccountID, len(pbs_resp.Bids))
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(pbs_resp)
	deps.m.RequestTimer.UpdateSince(pbs_req.Start)
}

// checkForValidBidSize goes through list of bids & find those which are banner mediaType and with height or width not defined
// determine the num of ad unit sizes that were used in corresponding bid request
// if num_adunit_sizes == 1, assign the height and/or width to bid's height/width
// if num_adunit_sizes > 1, reject the bid (remove from list) and return an error
// return updated bid list object for next steps in auction
func checkForValidBidSize(bids pbs.PBSBidSlice, bidder *pbs.PBSBidder) pbs.PBSBidSlice {
	finalValidBids := make([]*pbs.PBSBid, len(bids))
	finalBidCounter := 0
bidLoop:
	for _, bid := range bids {
		if bid.CreativeMediaType == "banner" && (bid.Height == 0 || bid.Width == 0) {
			for _, adunit := range bidder.AdUnits {
				if adunit.BidID == bid.BidID && adunit.Code == bid.AdUnitCode {
					if len(adunit.Sizes) == 1 {
						bid.Width, bid.Height = adunit.Sizes[0].W, adunit.Sizes[0].H
						finalValidBids[finalBidCounter] = bid
						finalBidCounter = finalBidCounter + 1
					} else if len(adunit.Sizes) > 1 {
						glog.Warningf("Bid was rejected for bidder %s because no size was defined", bid.BidderCode)
					}
					continue bidLoop
				}
			}
		} else {
			finalValidBids[finalBidCounter] = bid
			finalBidCounter = finalBidCounter + 1
		}
	}
	return finalValidBids[:finalBidCounter]
}

// sortBidsAddKeywordsMobile sorts the bids and adds ad server targeting keywords to each bid.
// The bids are sorted by cpm to find the highest bid.
// The ad server targeting keywords are added to all bids, with specific keywords for the highest bid.
func sortBidsAddKeywordsMobile(bids pbs.PBSBidSlice, pbs_req *pbs.PBSRequest, priceGranularitySetting string) {
	if priceGranularitySetting == "" {
		priceGranularitySetting = defaultPriceGranularity
	}

	// record bids by ad unit code for sorting
	code_bids := make(map[string]pbs.PBSBidSlice, len(bids))
	for _, bid := range bids {
		code_bids[bid.AdUnitCode] = append(code_bids[bid.AdUnitCode], bid)
	}

	// loop through ad units to find top bid
	for _, unit := range pbs_req.AdUnits {
		bar := code_bids[unit.Code]

		if len(bar) == 0 {
			if glog.V(3) {
				glog.Infof("No bids for ad unit '%s'", unit.Code)
			}
			continue
		}
		sort.Sort(bar)

		// after sorting we need to add the ad targeting keywords
		for i, bid := range bar {
			priceBucketStringMap := pbs.GetPriceBucketString(bid.Price)
			roundedCpm := priceBucketStringMap[priceGranularitySetting]

			hbSize := ""
			if bid.Width != 0 && bid.Height != 0 {
				width := strconv.FormatUint(bid.Width, 10)
				height := strconv.FormatUint(bid.Height, 10)
				hbSize = width + "x" + height
			}

			hbPbBidderKey := hbpbConstantKey + "_" + bid.BidderCode
			hbBidderBidderKey := hbBidderConstantKey + "_" + bid.BidderCode
			hbCacheIdBidderKey := hbCacheIdConstantKey + "_" + bid.BidderCode
			hbSizeBidderKey := hbSizeConstantKey + "_" + bid.BidderCode
			if pbs_req.MaxKeyLength != 0 {
				hbPbBidderKey = hbPbBidderKey[:min(len(hbPbBidderKey), int(pbs_req.MaxKeyLength))]
				hbBidderBidderKey = hbBidderBidderKey[:min(len(hbBidderBidderKey), int(pbs_req.MaxKeyLength))]
				hbCacheIdBidderKey = hbCacheIdBidderKey[:min(len(hbCacheIdBidderKey), int(pbs_req.MaxKeyLength))]
				hbSizeBidderKey = hbSizeBidderKey[:min(len(hbSizeBidderKey), int(pbs_req.MaxKeyLength))]
			}
			pbs_kvs := map[string]string{
				hbPbBidderKey:      roundedCpm,
				hbBidderBidderKey:  bid.BidderCode,
				hbCacheIdBidderKey: bid.CacheID,
			}
			if hbSize != "" {
				pbs_kvs[hbSizeBidderKey] = hbSize
			}
			// For the top bid, we want to add the following additional keys
			if i == 0 {
				pbs_kvs[hbpbConstantKey] = roundedCpm
				pbs_kvs[hbBidderConstantKey] = bid.BidderCode
				pbs_kvs[hbCacheIdConstantKey] = bid.CacheID
				if hbSize != "" {
					pbs_kvs[hbSizeConstantKey] = hbSize
				}
				if bid.BidderCode == "audienceNetwork" {
					pbs_kvs[hbCreativeLoadMethodConstantKey] = hbCreativeLoadMethodDemandSDK
				} else {
					pbs_kvs[hbCreativeLoadMethodConstantKey] = hbCreativeLoadMethodHTML
				}
			}
			bid.AdServerTargeting = pbs_kvs
		}
	}
}

func status(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	// could add more logic here, but doing nothing means 200 OK
}

// NewJsonDirectoryServer is used to serve .json files from a directory as a single blob. For example,
// given a directory containing the files "a.json" and "b.json", this returns a Handle which serves JSON like:
//
// {
//   "a": { ... content from the file a.json ... },
//   "b": { ... content from the file b.json ... }
// }
//
// This function stores the file contents in memory, and should not be used on large directories.
// If the root directory, or any of the files in it, cannot be read, then the program will exit.
func NewJsonDirectoryServer(schemaDirectory string) httprouter.Handle {
	// Slurp the files into memory first, since they're small and it minimizes request latency.
	files, err := ioutil.ReadDir(schemaDirectory)
	if err != nil {
		glog.Fatalf("Failed to read directory %s: %v", schemaDirectory, err)
	}

	data := make(map[string]json.RawMessage, len(files))
	for _, file := range files {
		bytes, err := ioutil.ReadFile(fmt.Sprintf("%s/%s", schemaDirectory, file.Name()))
		if err != nil {
			glog.Fatalf("Failed to read file %s/%s: %v", schemaDirectory, file.Name(), err)
		}
		data[file.Name()[0:len(file.Name())-5]] = json.RawMessage(bytes)
	}
	response, err := json.Marshal(data)
	if err != nil {
		glog.Fatalf("Failed to marshal bidder param JSON-schema: %v", err)
	}

	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Add("Content-Type", "application/json")
		w.Write(response)
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	http.ServeFile(w, r, "static/index.html")
}

type NoCache struct {
	handler http.Handler
}

func (m NoCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Add("Pragma", "no-cache")
	w.Header().Add("Expires", "0")
	m.handler.ServeHTTP(w, r)
}

// https://blog.golang.org/context/userip/userip.go
func getIP(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if ua := useragent.Parse(req.Header.Get("User-Agent")); ua != nil {
		fmt.Fprintf(w, "User Agent: %v\n", ua)
	}
	ip, port, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		fmt.Fprintf(w, "userip: %q is not IP:port\n", req.RemoteAddr)
	}

	userIP := net.ParseIP(ip)
	if userIP == nil {
		//return nil, fmt.Errorf("userip: %q is not IP:port", req.RemoteAddr)
		fmt.Fprintf(w, "userip: %q is not IP:port\n", req.RemoteAddr)
		return
	}

	forwardedIP := prebid.GetForwardedIP(req)
	realIP := prebid.GetIP(req)

	fmt.Fprintf(w, "IP: %s\n", ip)
	fmt.Fprintf(w, "Port: %s\n", port)
	fmt.Fprintf(w, "Forwarded IP: %s\n", forwardedIP)
	fmt.Fprintf(w, "Real IP: %s\n", realIP)

	for k, v := range req.Header {
		fmt.Fprintf(w, "%s: %s\n", k, v)
	}

}

func validate(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	w.Header().Add("Content-Type", "text/plain")
	defer r.Body.Close()
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		fmt.Fprintf(w, "Unable to read body\n")
		return
	}

	if reqSchema == nil {
		fmt.Fprintf(w, "Validation schema not loaded\n")
		return
	}

	js := gojsonschema.NewStringLoader(string(b))
	result, err := reqSchema.Validate(js)
	if err != nil {
		fmt.Fprintf(w, "Error parsing json: %v\n", err)
		return
	}

	if result.Valid() {
		fmt.Fprintf(w, "Validation successful\n")
		return
	}

	for _, err := range result.Errors() {
		fmt.Fprintf(w, "Error: %s %v\n", err.Context().String(), err)
	}

	return
}

func loadPostgresDataCache(cfg *config.Configuration) (cache.Cache, error) {
	mem := sigar.Mem{}
	mem.Get()

	return postgrescache.New(postgrescache.PostgresConfig{
		Dbname:   cfg.DataCache.Database,
		Host:     cfg.DataCache.Host,
		User:     cfg.DataCache.Username,
		Password: cfg.DataCache.Password,
		Size:     cfg.DataCache.CacheSize,
		TTL:      cfg.DataCache.TTLSeconds,
	})

}

func loadDataCache(cfg *config.Configuration) (err error) {

	switch cfg.DataCache.Type {
	case "dummy":
		dataCache, err = dummycache.New()
		if err != nil {
			glog.Fatalf("Dummy cache not configured: %s", err.Error())
		}

	case "postgres":
		dataCache, err = loadPostgresDataCache(cfg)
		if err != nil {
			return fmt.Errorf("PostgresCache Error: %s", err.Error())
		}

	case "filecache":
		dataCache, err = filecache.New(cfg.DataCache.Filename)
		if err != nil {
			return fmt.Errorf("FileCache Error: %s", err.Error())
		}

	default:
		return fmt.Errorf("Unknown datacache.type: %s", cfg.DataCache.Type)
	}
	return nil
}

func init() {
	rand.Seed(time.Now().UnixNano())
	viper.SetConfigName("pbs")
	viper.AddConfigPath(".")
	viper.AddConfigPath("/etc/config")

	viper.SetDefault("external_url", "http://localhost:8000")
	viper.SetDefault("port", 8000)
	viper.SetDefault("admin_port", 6060)
	viper.SetDefault("default_timeout_ms", 250)
	viper.SetDefault("datacache.type", "dummy")
	// no metrics configured by default (metrics{host|database|username|password})

	viper.SetDefault("adapters.pubmatic.endpoint", "http://openbid.pubmatic.com/translator?source=prebid-server")
	viper.SetDefault("adapters.rubicon.endpoint", "http://staged-by.rubiconproject.com/a/api/exchange.json")
	viper.SetDefault("adapters.rubicon.usersync_url", "https://pixel.rubiconproject.com/exchange/sync.php?p=prebid")
	viper.SetDefault("adapters.pulsepoint.endpoint", "http://bid.contextweb.com/header/s/ortb/prebid-s2s")
	viper.SetDefault("adapters.index.usersync_url", "//ssum-sec.casalemedia.com/usermatchredir?s=184932&cb=https%3A%2F%2Fprebid.adnxs.com%2Fpbs%2Fv1%2Fsetuid%3Fbidder%3DindexExchange%26uid%3D")
	viper.ReadInConfig()

	flag.Parse() // read glog settings from cmd line
}

func main() {
	cfg, err := config.New()
	if err != nil {
		glog.Errorf("Viper was unable to read configurations: %v", err)
	}

	if err := serve(cfg); err != nil {
		glog.Errorf("prebid-server failed: %v", err)
	}
}

func setupExchanges(cfg *config.Configuration) {
	exchanges = map[string]adapters.Adapter{
		"appnexus":      adapters.NewAppNexusAdapter(adapters.DefaultHTTPAdapterConfig, cfg.ExternalURL),
		"districtm":     adapters.NewAppNexusAdapter(adapters.DefaultHTTPAdapterConfig, cfg.ExternalURL),
		"indexExchange": adapters.NewIndexAdapter(adapters.DefaultHTTPAdapterConfig, cfg.Adapters["indexexchange"].Endpoint, cfg.Adapters["indexexchange"].UserSyncURL),
		"pubmatic":      adapters.NewPubmaticAdapter(adapters.DefaultHTTPAdapterConfig, cfg.Adapters["pubmatic"].Endpoint, cfg.ExternalURL),
		"pulsepoint":    adapters.NewPulsePointAdapter(adapters.DefaultHTTPAdapterConfig, cfg.Adapters["pulsepoint"].Endpoint, cfg.ExternalURL),
		"rubicon": adapters.NewRubiconAdapter(adapters.DefaultHTTPAdapterConfig, cfg.Adapters["rubicon"].Endpoint,
			cfg.Adapters["rubicon"].XAPI.Username, cfg.Adapters["rubicon"].XAPI.Password, cfg.Adapters["rubicon"].XAPI.Tracker, cfg.Adapters["rubicon"].UserSyncURL),
		"audienceNetwork": adapters.NewFacebookAdapter(adapters.DefaultHTTPAdapterConfig, cfg.Adapters["facebook"].PlatformID, cfg.Adapters["facebook"].UserSyncURL),
		"lifestreet":      adapters.NewLifestreetAdapter(adapters.DefaultHTTPAdapterConfig, cfg.ExternalURL),
	}
}

func serve(cfg *config.Configuration) error {
	if err := loadDataCache(cfg); err != nil {
		return fmt.Errorf("Prebid Server could not load data cache: %v", err)
	}

	setupExchanges(cfg)

	m := pbsmetrics.NewMetrics(keys(exchanges))
	if cfg.Metrics.Host != "" {
		go m.Export(cfg)
	}

	b, err := ioutil.ReadFile("static/pbs_request.json")
	if err != nil {
		glog.Errorf("Unable to open pbs_request.json: %v", err)
	} else {
		sl := gojsonschema.NewStringLoader(string(b))
		reqSchema, err = gojsonschema.NewSchema(sl)
		if err != nil {
			glog.Errorf("Unable to load request schema: %v", err)
		}
	}

	stopSignals := make(chan os.Signal)
	signal.Notify(stopSignals, syscall.SIGTERM, syscall.SIGINT)

	/* Run admin on different port thats not exposed */
	adminURI := fmt.Sprintf("%s:%d", cfg.Host, cfg.AdminPort)
	adminServer := &http.Server{Addr: adminURI}
	go (func() {
		fmt.Println("Admin running on: ", adminURI)
		err := adminServer.ListenAndServe()
		glog.Errorf("Admin server: %v", err)
		stopSignals <- syscall.SIGTERM
	})()

	router := httprouter.New()
	router.POST("/auction", (&auctionDeps{m}).auction)
	router.GET("/bidders/params", NewJsonDirectoryServer(schemaDirectory))
	router.POST("/cookie_sync", (&cookieSyncDeps{m}).cookieSync)
	router.POST("/validate", validate)
	router.GET("/status", status)
	router.GET("/", serveIndex)
	router.GET("/ip", getIP)
	router.ServeFiles("/static/*filepath", http.Dir("static"))

	hostCookieSettings = pbs.HostCookieSettings{
		Domain:     cfg.HostCookie.Domain,
		Family:     cfg.HostCookie.Family,
		CookieName: cfg.HostCookie.CookieName,
		OptOutURL:  cfg.HostCookie.OptOutURL,
		OptInURL:   cfg.HostCookie.OptInURL,
	}

	userSyncDeps := &pbs.UserSyncDeps{
		HostCookieSettings: &hostCookieSettings,
		ExternalUrl:        cfg.ExternalURL,
		RecaptchaSecret:    cfg.RecaptchaSecret,
		Metrics:            m,
	}

	router.GET("/getuids", userSyncDeps.GetUIDs)
	router.GET("/setuid", userSyncDeps.SetUID)
	router.POST("/optout", userSyncDeps.OptOut)
	router.GET("/optout", userSyncDeps.OptOut)

	pbc.InitPrebidCache(cfg.CacheURL)

	// Add CORS middleware
	c := cors.New(cors.Options{AllowCredentials: true})
	corsRouter := c.Handler(router)

	// Add no cache headers
	noCacheHandler := NoCache{corsRouter}

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      noCacheHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	go (func() {
		fmt.Printf("Main server running on: %s\n", server.Addr)
		serverErr := server.ListenAndServe()
		glog.Errorf("Main server: %v", serverErr)
		stopSignals <- syscall.SIGTERM
	})()

	<-stopSignals

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		glog.Errorf("Main server shutdown: %v", err)
	}
	if err := adminServer.Shutdown(ctx); err != nil {
		glog.Errorf("Admin server shutdown: %v", err)
	}

	return nil
}

func keys(m map[string]adapters.Adapter) []string {
	keys := make([]string, len(m))

	i := 0
	for k := range m {
		keys[i] = k
		i++
	}
	return keys
}
