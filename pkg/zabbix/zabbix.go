package zabbix

import (
	"context"
	"strings"
	"time"

	"github.com/alexanderzobnin/grafana-zabbix/pkg/settings"
	"github.com/alexanderzobnin/grafana-zabbix/pkg/zabbixapi"
	"github.com/bitly/go-simplejson"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// Zabbix is a wrapper for Zabbix API. It wraps Zabbix API queries and performs authentication, adds caching,
// deduplication and other performance optimizations.
type Zabbix struct {
	api     *zabbixapi.ZabbixAPI
	dsInfo  *backend.DataSourceInstanceSettings
	cache   *ZabbixCache
	version int
	logger  log.Logger
}

// New returns new instance of Zabbix client.
func New(dsInfo *backend.DataSourceInstanceSettings, zabbixSettings *settings.ZabbixDatasourceSettings, zabbixAPI *zabbixapi.ZabbixAPI) (*Zabbix, error) {
	logger := log.New()
	zabbixCache := NewZabbixCache(zabbixSettings.CacheTTL, 10*time.Minute)

	return &Zabbix{
		api:    zabbixAPI,
		dsInfo: dsInfo,
		cache:  zabbixCache,
		logger: logger,
	}, nil
}

func (zabbix *Zabbix) GetAPI() *zabbixapi.ZabbixAPI {
	return zabbix.api
}

// Request wraps request with cache
func (ds *Zabbix) Request(ctx context.Context, apiReq *ZabbixAPIRequest) (*simplejson.Json, error) {
	var resultJson *simplejson.Json
	var err error

	if ds.version == 0 {
		version, err := ds.GetVersion(ctx)
		if err != nil {
			ds.logger.Error("Error querying Zabbix version", "error", err)
			ds.version = -1
		} else {
			ds.logger.Debug("Got Zabbix version", "version", version)
			ds.version = version
		}
	}

	cachedResult, queryExistInCache := ds.cache.GetAPIRequest(apiReq)
	if !queryExistInCache {
		resultJson, err = ds.request(ctx, apiReq.Method, apiReq.Params)
		if err != nil {
			return nil, err
		}

		if IsCachedRequest(apiReq.Method) {
			ds.logger.Debug("Writing result to cache", "method", apiReq.Method)
			ds.cache.SetAPIRequest(apiReq, resultJson)
		}
	} else {
		var ok bool
		resultJson, ok = cachedResult.(*simplejson.Json)
		if !ok {
			resultJson = simplejson.New()
		}
	}

	return resultJson, nil
}

// request checks authentication and makes a request to the Zabbix API.
func (zabbix *Zabbix) request(ctx context.Context, method string, params ZabbixAPIParams) (*simplejson.Json, error) {
	zabbix.logger.Debug("Zabbix request", "method", method)

	// Skip auth for methods that are not required it
	if method == "apiinfo.version" {
		return zabbix.api.RequestUnauthenticated(ctx, method, params)
	}

	result, err := zabbix.api.Request(ctx, method, params)
	notAuthorized := isNotAuthorized(err)
	if err == zabbixapi.ErrNotAuthenticated || notAuthorized {
		if notAuthorized {
			zabbix.logger.Debug("Authentication token expired, performing re-login")
		}
		err = zabbix.Login(ctx)
		if err != nil {
			return nil, err
		}
		return zabbix.request(ctx, method, params)
	} else if err != nil {
		return nil, err
	}

	return result, err
}

func (zabbix *Zabbix) Login(ctx context.Context) error {
	jsonData, err := simplejson.NewJson(zabbix.dsInfo.JSONData)
	if err != nil {
		return err
	}

	zabbixLogin := jsonData.Get("username").MustString()
	var zabbixPassword string
	if securePassword, exists := zabbix.dsInfo.DecryptedSecureJSONData["password"]; exists {
		zabbixPassword = securePassword
	} else {
		// Fallback
		zabbixPassword = jsonData.Get("password").MustString()
	}

	err = zabbix.api.Authenticate(ctx, zabbixLogin, zabbixPassword)
	if err != nil {
		zabbix.logger.Error("Zabbix authentication error", "error", err)
		return err
	}
	zabbix.logger.Debug("Successfully authenticated", "url", zabbix.api.GetUrl().String(), "user", zabbixLogin)

	return nil
}

func isNotAuthorized(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()
	return strings.Contains(message, "Session terminated, re-login, please.") ||
		strings.Contains(message, "Not authorised.") ||
		strings.Contains(message, "Not authorized.")
}
