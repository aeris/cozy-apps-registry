package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-apps-registry/auth"
	"github.com/cozy/cozy-apps-registry/errshttp"
	"github.com/cozy/cozy-apps-registry/registry"

	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"

	"github.com/go-kivik/kivik"
)

const RegistryVersion = "0.1.0"

const authTokenScheme = "Token "
const contextKey = "context"

var queryFilterReg = regexp.MustCompile(`^filter\[([a-z]+)\]$`)

var (
	oneMinute = 1 * time.Minute
	oneHour   = 1 * time.Hour
	oneYear   = 365 * 24 * time.Hour
)

func createApp(c echo.Context) (err error) {
	if err = checkAuthorized(c); err != nil {
		return err
	}

	opts := &registry.AppOptions{}
	if err = c.Bind(opts); err != nil {
		return err
	}

	if err = validateAppRequest(c, opts); err != nil {
		return err
	}

	editor, err := checkPermissions(c, opts.Editor)
	if err != nil {
		return err
	}

	app, err := registry.CreateApp(getContext(c), opts, editor)
	if err != nil {
		return err
	}

	// Do not show internal identifier and revision
	app.ID = ""
	app.Rev = ""

	return c.JSON(http.StatusCreated, app)
}

func createVersion(c echo.Context) (err error) {
	if err = checkAuthorized(c); err != nil {
		return err
	}

	appSlug := c.Param("app")
	app, err := registry.FindApp(getContext(c), appSlug)
	if err != nil {
		return err
	}

	opts := &registry.VersionOptions{}
	if err = c.Bind(opts); err != nil {
		return err
	}
	opts.Version = stripVersion(opts.Version)

	if err = validateVersionRequest(c, opts); err != nil {
		return err
	}

	_, err = registry.FindVersion(getContext(c), appSlug, opts.Version)
	if err == nil {
		return registry.ErrVersionAlreadyExists
	}
	if err != registry.ErrVersionNotFound {
		return err
	}

	ver, err := registry.DownloadVersion(opts)
	if err != nil {
		return err
	}

	editor, err := checkPermissions(c, ver.Editor)
	if err != nil {
		return err
	}

	if err = registry.CreateVersion(getContext(c), ver, app, editor); err != nil {
		return err
	}

	// Do not show internal identifier and revision
	ver.ID = ""
	ver.Rev = ""
	ver.Attachments = nil

	return c.JSON(http.StatusCreated, ver)
}

func checkAuthorized(c echo.Context) error {
	token, err := extractAuthHeader(c)
	if err != nil {
		return err
	}
	if _, ok := auth.VerifyToken(sessionSecret, token, nil); !ok {
		return errshttp.NewError(http.StatusUnauthorized, "Token could not be verified")
	}
	return nil
}

func checkPermissions(c echo.Context, editorName string) (*auth.Editor, error) {
	token, err := extractAuthHeader(c)
	if err != nil {
		return nil, err
	}

	editor, err := editorRegistry.GetEditor(editorName)
	if err != nil {
		return nil, errshttp.NewError(http.StatusUnauthorized, "Could not find editor: %s", editorName)
	}

	ok := editor.VerifySessionToken(sessionSecret, token)
	if !ok {
		return nil, errshttp.NewError(http.StatusUnauthorized, "Token could not be verified")
	}
	return editor, nil
}

func extractAuthHeader(c echo.Context) ([]byte, error) {
	authHeader := c.Request().Header.Get(echo.HeaderAuthorization)
	if !strings.HasPrefix(authHeader, authTokenScheme) {
		return nil, errshttp.NewError(http.StatusUnauthorized, "Missing prefix from authorization header")
	}
	tokenStr := authHeader[len(authTokenScheme):]
	if len(tokenStr) > 1024 { // tokens should be much less than 128bytes
		return nil, errshttp.NewError(http.StatusUnauthorized, "Token is too long")
	}
	token, err := base64.StdEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, errshttp.NewError(http.StatusUnauthorized, "Token is not properly base64 encoded")
	}
	return token, nil
}

func getAppsList(c echo.Context) error {
	var filter map[string]string
	var limit, cursor int
	var sort string
	var err error
	for name, vals := range c.QueryParams() {
		val := vals[0]
		switch name {
		case "limit":
			limit, err = strconv.Atoi(val)
			if err != nil {
				return errshttp.NewError(http.StatusBadRequest,
					`Query param "limit" is invalid: %s`, err)
			}
		case "cursor":
			cursor, err = strconv.Atoi(val)
			if err != nil {
				return errshttp.NewError(http.StatusBadRequest,
					`Query param "cursor" is invalid: %s`, err)
			}
		case "sort":
			sort = val
		default:
			if queryFilterReg.MatchString(name) {
				subs := queryFilterReg.FindStringSubmatch(name)
				if len(subs) == 2 {
					if filter == nil {
						filter = make(map[string]string)
					}
					filter[subs[1]] = val
				}
			}
		}
	}

	next, docs, err := registry.GetAppsList(getContext(c), &registry.AppsListOptions{
		Filters: filter,
		Limit:   limit,
		Cursor:  cursor,
		Sort:    sort,
	})
	if err != nil {
		return err
	}

	for _, doc := range docs {
		// Do not show internal identifier and revision
		doc.ID = ""
		doc.Rev = ""
	}

	type pageInfo struct {
		Count      int    `json:"count"`
		NextCursor string `json:"next_cursor,omitempty"`
	}

	var nextCursor string
	if next >= 0 {
		nextCursor = strconv.Itoa(next)
	}

	j := struct {
		List     []*registry.App `json:"data"`
		PageInfo pageInfo        `json:"meta"`
	}{
		List: docs,
		PageInfo: pageInfo{
			Count:      len(docs),
			NextCursor: nextCursor,
		},
	}

	return json(c, j)
}

func getApp(c echo.Context) error {
	appSlug := c.Param("app")
	doc, err := registry.FindApp(getContext(c), appSlug)
	if err != nil {
		return err
	}

	if cacheControl(c, doc.Rev, oneMinute) {
		return c.NoContent(http.StatusNotModified)
	}

	// Do not show internal identifier and revision
	doc.ID = ""
	doc.Rev = ""

	return json(c, doc)
}

func getAppIcon(c echo.Context) error {
	return getAppAttachment(c, "icon")
}

func getAppScreenshot(c echo.Context) error {
	return getAppAttachment(c, path.Join("screenshots", c.Param("filename")))
}

func getAppAttachment(c echo.Context, filename string) error {
	appSlug := c.Param("app")
	channel := c.Param("channel")

	var att *kivik.Attachment
	{
		if channel == "" {
			var err error
			for _, ch := range []registry.Channel{registry.Stable, registry.Beta, registry.Dev} {
				att, err = registry.FindAppAttachment(getContext(c), appSlug, filename, ch)
				if err == nil {
					break
				}
				if err != registry.ErrVersionNotFound {
					return err
				}
			}
			if att == nil {
				return echo.NewHTTPError(http.StatusNotFound)
			}
		} else {
			ch, err := registry.StrToChannel(channel)
			if err != nil {
				ch = registry.Stable
			}
			att, err = registry.FindAppAttachment(getContext(c), appSlug, filename, ch)
			if err != nil {
				return err
			}
		}
		defer att.Content.Close()
	}

	if cacheControl(c, att.Digest, oneHour) {
		return c.NoContent(http.StatusNotModified)
	}

	if c.Request().Method == http.MethodHead {
		c.Response().Header().Set(echo.HeaderContentType, att.ContentType)
		return c.NoContent(http.StatusOK)
	}
	return c.Stream(http.StatusOK, att.ContentType, att.Content)
}

func getVersionIcon(c echo.Context) error {
	return getVersionAttachment(c, "icon")
}

func getVersionScreenshot(c echo.Context) error {
	return getVersionAttachment(c, path.Join("screenshots", c.Param("filename")))
}

func getVersionAttachment(c echo.Context, filename string) error {
	appSlug := c.Param("app")
	version := c.Param("version")
	att, err := registry.FindVersionAttachment(getContext(c), appSlug, version, filename)
	if err != nil {
		return err
	}
	defer att.Content.Close()

	c.Response().Header().Set(echo.HeaderContentType, att.ContentType)
	if cacheControl(c, att.Digest, oneHour) {
		return c.NoContent(http.StatusNotModified)
	}

	if c.Request().Method == http.MethodHead {
		return c.NoContent(http.StatusOK)
	}
	return c.Stream(http.StatusOK, att.ContentType, att.Content)
}

func getAppVersions(c echo.Context) error {
	appSlug := c.Param("app")
	doc, err := registry.FindAppVersions(getContext(c), appSlug)
	if err != nil {
		return err
	}

	if cacheControl(c, "", oneMinute) {
		return c.NoContent(http.StatusNotModified)
	}

	return json(c, doc)
}

func getVersion(c echo.Context) error {
	appSlug := c.Param("app")
	version := stripVersion(c.Param("version"))
	_, err := registry.FindApp(getContext(c), appSlug)
	if err != nil {
		return err
	}

	doc, err := registry.FindVersion(getContext(c), appSlug, version)
	if err != nil {
		return err
	}

	if cacheControl(c, doc.Rev, oneYear) {
		return c.NoContent(http.StatusNotModified)
	}

	// Do not show internal identifier and revision
	doc.ID = ""
	doc.Rev = ""
	doc.Attachments = nil

	return json(c, doc)
}

func getLatestVersion(c echo.Context) error {
	appSlug := c.Param("app")
	channel := c.Param("channel")

	ch, err := registry.StrToChannel(channel)
	if err != nil {
		return err
	}
	doc, err := registry.FindLatestVersion(getContext(c), appSlug, ch)
	if err != nil {
		return err
	}

	if cacheControl(c, doc.Rev, oneMinute) {
		return c.NoContent(http.StatusNotModified)
	}

	// Do not show internal identifier and revision
	doc.ID = ""
	doc.Rev = ""
	doc.Attachments = nil

	return json(c, doc)
}

func getEditor(c echo.Context) error {
	editorName := c.Param("editor")
	editor, err := editorRegistry.GetEditor(editorName)
	if err != nil {
		return err
	}

	if cacheControl(c, "", oneMinute) {
		return c.NoContent(http.StatusNotModified)
	}

	return json(c, editor)
}

func getEditorsList(c echo.Context) error {
	editors, err := editorRegistry.AllEditors()
	if err != nil {
		return err
	}
	return json(c, editors)
}

// jsonEndPoint middleware checks that the Content-Type and Accept headers are
// properly set for an application/json endpoint.
func jsonEndpoint(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Set("json", true)
		req := c.Request()
		switch req.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			contentType := c.Request().Header.Get(echo.HeaderContentType)
			if !strings.HasPrefix(contentType, echo.MIMEApplicationJSON) {
				return errshttp.NewError(http.StatusUnsupportedMediaType,
					"Content-Type should be application/json")
			}
		}
		acceptHeader := req.Header.Get("Accept")
		if acceptHeader != "" &&
			!strings.Contains(acceptHeader, echo.MIMEApplicationJSON) &&
			!strings.Contains(acceptHeader, "*/*") {
			return errshttp.NewError(http.StatusNotAcceptable,
				"Accept header does not contain application/json")
		}
		return next(c)
	}
}

func ensureContext(contextName string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			context, ok := registry.GetContext(contextName)
			if !ok {
				return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Context %q does not exist", contextName))
			}
			c.Set(contextKey, context)
			return next(c)
		}
	}
}

func getContext(c echo.Context) *registry.Context {
	return c.Get(contextKey).(*registry.Context)
}

func validateAppRequest(c echo.Context, app *registry.AppOptions) error {
	if err := registry.IsValidApp(app); err != nil {
		return wrapErr(err, http.StatusBadRequest)
	}
	return nil
}

func validateVersionRequest(c echo.Context, ver *registry.VersionOptions) error {
	if err := registry.IsValidVersion(ver); err != nil {
		return wrapErr(err, http.StatusBadRequest)
	}
	return nil
}

func httpErrorHandler(err error, c echo.Context) {
	var (
		code = http.StatusInternalServerError
		msg  string
	)

	isJSON, _ := c.Get("json").(bool)

	if he, ok := err.(*errshttp.Error); ok {
		code = he.StatusCode()
		msg = err.Error()
	} else if he, ok := err.(*echo.HTTPError); ok {
		code = he.Code
		msg = fmt.Sprintf("%s", he.Message)
	} else {
		msg = err.Error()
	}

	respHeaders := c.Response().Header()
	switch err {
	case registry.ErrVersionNotFound, registry.ErrAppNotFound:
		respHeaders.Set("cache-control", "max-age=60")
	default:
		respHeaders.Set("cache-control", "no-cache")
	}

	if !c.Response().Committed {
		if isJSON {
			if c.Request().Method == echo.HEAD {
				c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
				c.NoContent(code)
			} else {
				c.JSON(code, echo.Map{"error": msg})
			}
		} else {
			if c.Request().Method == echo.HEAD {
				c.NoContent(code)
			} else {
				c.String(code, msg)
			}
		}
	}
}

func wrapErr(err error, code int) error {
	if err == nil {
		return nil
	}
	if errHTTP, ok := err.(*errshttp.Error); ok {
		return errHTTP
	}
	return errshttp.NewError(code, err.Error())
}

func cacheControl(c echo.Context, rev string, maxAge time.Duration) bool {
	headers := c.Response().Header()
	headers.Set("cache-control", fmt.Sprintf("max-age=%d", int(maxAge.Seconds())))
	headers.Set("date", time.Now().UTC().Format(http.TimeFormat))

	if rev != "" {
		headers.Set("etag", rev)
		revMatches := strings.Split(c.Request().Header.Get("if-none-match"), ",")
		for _, revMatch := range revMatches {
			if strings.TrimSpace(revMatch) == rev {
				return true
			}
		}
	}

	return false
}

// stripVersion removes the 'v' prefix if any.
// ex: v1.3.2 -> 1.3.2
func stripVersion(v string) string {
	if len(v) > 0 && v[0] == 'v' {
		v = v[1:]
	}
	return v
}

func json(c echo.Context, doc interface{}) error {
	if c.Request().Method == http.MethodHead {
		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
		return c.NoContent(http.StatusOK)
	}
	return c.JSON(http.StatusOK, doc)
}

func Router(addr string) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = httpErrorHandler

	e.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Add("X-Apps-Registry-Version", RegistryVersion)
			return next(c)
		}
	})
	e.Pre(middleware.RemoveTrailingSlash())
	e.Use(middleware.BodyLimit("100K"))
	e.Use(middleware.Logger())
	e.Use(middleware.Gzip())
	e.Use(middleware.Recover())

	{
		var g *echo.Group
		for _, c := range registry.GetContextsNames() {
			g = e.Group(c+"/registry", ensureContext(c))

			g.POST("", createApp, jsonEndpoint)
			g.POST("/:app", createVersion, jsonEndpoint)

			g.GET("", getAppsList, jsonEndpoint)
			g.HEAD("/:app", getApp, jsonEndpoint)
			g.GET("/:app", getApp, jsonEndpoint)
			g.GET("/:app/versions", getAppVersions, jsonEndpoint)
			g.HEAD("/:app/:version", getVersion, jsonEndpoint)
			g.GET("/:app/:version", getVersion, jsonEndpoint)
			g.HEAD("/:app/:channel/latest", getLatestVersion, jsonEndpoint)
			g.GET("/:app/:channel/latest", getLatestVersion, jsonEndpoint)

			g.GET("/:app/icon", getAppIcon)
			g.HEAD("/:app/icon", getAppIcon)
			g.GET("/:app/screenshots/:filename", getAppScreenshot)
			g.HEAD("/:app/screenshots/:filename", getAppScreenshot)
			g.GET("/:app/:channel/latest/icon", getAppIcon)
			g.HEAD("/:app/:channel/latest/icon", getAppIcon)
			g.HEAD("/:app/:version/icon", getVersionIcon)
			g.GET("/:app/:version/icon", getVersionIcon)
			g.HEAD("/:app/:version/screenshots/:filename", getVersionScreenshot)
			g.GET("/:app/:version/screenshots/:filename", getVersionScreenshot)
		}
	}

	e.GET("/editors", getEditorsList, jsonEndpoint)
	e.HEAD("/editors/:editor", getEditor, jsonEndpoint)
	e.GET("/editors/:editor", getEditor, jsonEndpoint)

	return e
}
