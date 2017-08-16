package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cozy/cozy-registry-v3/auth"
	"github.com/cozy/cozy-registry-v3/errshttp"
	"github.com/cozy/echo"

	"github.com/flimzy/kivik"
	_ "github.com/flimzy/kivik/driver/couchdb" // for couchdb
)

const maxApplicationSize = 20 * 1024 * 1024 // 20 Mo

var (
	validAppNameReg = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9\-]*$`)
	validVersionReg = regexp.MustCompile(`^(0|[1-9][0-9]{0,4})\.(0|[1-9][0-9]{0,4})\.(0|[1-9][0-9]{0,4})(-dev\.[a-f0-9]{5,40}|-beta.(0|[1-9][0-9]{0,4}))?$`)

	validAppTypes = []string{"webapp", "konnector"}
)

var (
	ErrAppNotFound     = errshttp.NewError(http.StatusNotFound, "Application was not found")
	ErrAppNameMismatch = errshttp.NewError(http.StatusBadRequest, "Application name does not match the one specified in the body")
	ErrAppInvalid      = errshttp.NewError(http.StatusBadRequest, "Invalid application name: should contain only alphanumeric characters and dashes")

	ErrVersionAlreadyExists = errshttp.NewError(http.StatusConflict, "Version already exists")
	ErrVersionNotFound      = errshttp.NewError(http.StatusNotFound, "Version was not found")
	ErrVersionMismatch      = errshttp.NewError(http.StatusBadRequest, "Version does not match the one specified in the body")
	ErrVersionInvalid       = errshttp.NewError(http.StatusBadRequest, "Invalid version value")
	ErrChannelInvalid       = errshttp.NewError(http.StatusBadRequest, `Invalid version channel: should be "stable", "beta" or "dev"`)
)

var versionClient = http.Client{
	Timeout: 20 * time.Second,
}

const (
	AppsDB    = "registry-apps"
	VersDB    = "registry-versions"
	EditorsDB = "registry-editors"

	devSuffix  = "-dev."
	betaSuffix = "-beta."
)

var (
	client *kivik.Client

	ctx = context.Background()
	dbs = []string{AppsDB, VersDB, EditorsDB}

	appsIndex = echo.Map{"fields": []string{"name", "type", "editor", "category", "tags"}}
	versIndex = echo.Map{"fields": []string{"version", "name", "type"}}
)

type Channel string

const (
	Stable Channel = "stable"
	Beta   Channel = "beta"
	Dev    Channel = "dev"
)

type App struct {
	ID             string         `json:"_id,omitempty"`
	Rev            string         `json:"_rev,omitempty"`
	Name           string         `json:"name"`
	FullName       AppFullName    `json:"full_name"`
	Type           string         `json:"type"`
	Editor         string         `json:"editor"`
	Description    AppDescription `json:"description"`
	Category       string         `json:"category"`
	Repository     string         `json:"repository"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Tags           []string       `json:"tags"`
	LogoURL        string         `json:"logo_url"`
	ScreenshotURLs []string       `json:"screenshot_urls"`
	Versions       *AppVersions   `json:"versions,omitempty"`
}

type AppDescription map[string]string
type AppFullName map[string]string

type AppVersions struct {
	Stable []string `json:"stable"`
	Beta   []string `json:"beta"`
	Dev    []string `json:"dev"`
}

type Version struct {
	ID        string          `json:"_id,omitempty"`
	Rev       string          `json:"_rev,omitempty"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Version   string          `json:"version"`
	Manifest  json.RawMessage `json:"manifest"`
	CreatedAt time.Time       `json:"created_at"`
	URL       string          `json:"url"`
	Size      int64           `json:"size,string"`
	Sha256    string          `json:"sha256"`
	Signature []byte          `json:"signature,omitempty"`
	TarPrefix string          `json:"tar_prefix"`
}

func InitDBClient(addr, user, pass string) (*kivik.Client, error) {
	var err error

	var userInfo *url.Userinfo
	if user != "" {
		if pass != "" {
			userInfo = url.UserPassword(user, pass)
		} else {
			userInfo = url.User(user)
		}
	}

	client, err = kivik.New(ctx, "couch", (&url.URL{
		Scheme: "http",
		User:   userInfo,
		Host:   addr,
	}).String())
	if err != nil {
		return nil, fmt.Errorf("Could not reach CouchDB: %s", err.Error())
	}

	for _, dbName := range dbs {
		var ok bool
		ok, err = client.DBExists(ctx, dbName)
		if err != nil {
			return nil, err
		}
		if !ok {
			fmt.Printf("Creating database %s...", dbName)
			if err = client.CreateDB(ctx, dbName); err != nil {
				fmt.Println("failed")
				return nil, err
			}
			fmt.Println("ok")
		}
	}

	dbApps, err := client.DB(ctx, AppsDB)
	if err != nil {
		return nil, err
	}

	err = dbApps.CreateIndex(ctx, "apps-index", "apps-index", appsIndex)
	if err != nil {
		return nil, err
	}

	dbVers, err := client.DB(ctx, VersDB)
	if err != nil {
		return nil, err
	}

	err = dbVers.CreateIndex(ctx, "versions-index", "versions-index", versIndex)
	if err != nil {
		return nil, err
	}

	return client, err
}

func IsValidApp(app *App) error {
	var fields []string
	if app.Name == "" || !validAppNameReg.MatchString(app.Name) {
		return ErrAppInvalid
	}
	if app.Editor == "" {
		fields = append(fields, "editor")
	}
	if !stringInArray(app.Type, validAppTypes) {
		fields = append(fields, "type")
	}
	if app.Repository != "" {
		if _, err := url.Parse(app.Repository); err != nil {
			fields = append(fields, "repository")
		}
	}
	if len(fields) > 0 {
		return errshttp.NewError(http.StatusBadRequest, "Invalid application, "+
			"the following fields are missing or erroneous: %s", strings.Join(fields, ", "))
	}
	return nil
}

func IsValidVersion(ver *Version) error {
	if ver.Name == "" || !validAppNameReg.MatchString(ver.Name) {
		return ErrAppInvalid
	}
	if ver.Version == "" || !validVersionReg.MatchString(ver.Version) {
		return ErrVersionMismatch
	}
	var fields []string
	if ver.URL == "" {
		fields = append(fields, "url")
	} else if _, err := url.Parse(ver.URL); err != nil {
		fields = append(fields, "url")
	}
	if h, err := hex.DecodeString(ver.Sha256); err != nil || len(h) != 32 {
		fields = append(fields, "sha256")
	}
	if len(fields) > 0 {
		return fmt.Errorf("Invalid version, "+
			"the following fields are missing or erroneous: %s", strings.Join(fields, ", "))
	}
	return nil
}

func CreateOrUpdateApp(app *App, editor *auth.Editor) error {
	if err := IsValidApp(app); err != nil {
		return err
	}

	db, err := client.DB(ctx, AppsDB)
	if err != nil {
		return err
	}
	oldApp, err := FindApp(app.Name)
	if err != nil && err != ErrAppNotFound {
		return err
	}
	if err == ErrAppNotFound {
		now := time.Now()
		app.ID = getAppID(app.Name)
		app.Name = app.ID
		app.Editor = editor.Name()
		app.CreatedAt = now
		app.UpdatedAt = now
		app.Versions = nil
		if app.FullName == nil {
			app.FullName = make(AppFullName)
		}
		if app.Description == nil {
			app.Description = make(AppDescription)
		}
		if app.Tags == nil {
			app.Tags = make([]string, 0)
		}
		if app.ScreenshotURLs == nil {
			app.ScreenshotURLs = make([]string, 0)
		}
		_, _, err = db.CreateDoc(ctx, app)
		return err
	}
	app.ID = oldApp.ID
	app.Rev = oldApp.Rev
	app.Name = oldApp.Name
	app.Type = oldApp.Type
	app.Editor = editor.Name()
	app.CreatedAt = oldApp.CreatedAt
	app.UpdatedAt = time.Now()
	app.Versions = nil
	if app.Category == "" {
		app.Category = oldApp.Category
	}
	if app.Repository == "" {
		app.Repository = oldApp.Repository
	}
	if app.FullName == nil {
		app.FullName = oldApp.FullName
	}
	if app.Description == nil {
		app.Description = oldApp.Description
	}
	if app.Tags == nil {
		app.Tags = oldApp.Tags
	}
	if app.ScreenshotURLs == nil {
		app.ScreenshotURLs = oldApp.ScreenshotURLs
	}
	_, err = db.Put(ctx, app.ID, app)
	return err
}

func CreateVersion(ver *Version, editor *auth.Editor) error {
	if err := IsValidVersion(ver); err != nil {
		return err
	}

	app, err := FindApp(ver.Name)
	if err != nil {
		return err
	}
	_, err = FindVersion(ver.Name, ver.Version)
	if err != ErrVersionNotFound {
		if err == nil {
			return ErrVersionAlreadyExists
		}
		return err
	}

	ver.Type = app.Type

	man, prefix, size, err := downloadAndCheckVersion(app, ver, editor)
	if err != nil {
		return err
	}

	ver.ID = getVersionID(app.Name, ver.Version)
	ver.Name = app.Name
	ver.Manifest = man
	ver.Size = size
	ver.TarPrefix = prefix
	ver.CreatedAt = time.Now()

	db, err := client.DB(ctx, VersDB)
	if err != nil {
		return err
	}
	_, _, err = db.CreateDoc(ctx, ver)
	return err
}

func downloadAndCheckVersion(app *App, ver *Version, editor *auth.Editor) (manifestContent []byte, prefix string, size int64, err error) {
	req, err := http.NewRequest(http.MethodGet, ver.URL, nil)
	if err != nil {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Could not reach version on specified url %s: %s",
			ver.URL, err.Error())
		return
	}
	res, err := versionClient.Do(req)
	if err != nil {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Could not reach version on specified url %s: %s",
			ver.URL, err.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Could not reach version on specified url %s: server responded with code %d",
			ver.URL, res.StatusCode)
		return
	}

	h := sha256.New()
	var reader io.Reader
	counter := &Counter{}
	reader = io.LimitReader(res.Body, maxApplicationSize)
	reader = io.TeeReader(reader, counter)
	reader = io.TeeReader(reader, h)

	contentType := res.Header.Get("Content-Type")
	switch contentType {
	case
		"application/gzip",
		"application/x-gzip",
		"application/x-tgz",
		"application/tar+gzip":
		reader, err = gzip.NewReader(reader)
		if err != nil {
			err = errshttp.NewError(http.StatusUnprocessableEntity,
				"Could not reach version on specified url %s: %s",
				ver.URL, err.Error())
			return
		}
	case "application/octet-stream":
		var r io.Reader
		if r, err = gzip.NewReader(reader); err == nil {
			reader = r
		}
	}

	manName := getManifestName(ver.Type)
	tarReader := tar.NewReader(reader)
	for {
		var hdr *tar.Header
		hdr, err = tarReader.Next()
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			err = errshttp.NewError(http.StatusUnprocessableEntity,
				"Could not reach version on specified url %s: file is too big %s",
				ver.URL, err.Error())
			return
		}
		if err != nil {
			err = errshttp.NewError(http.StatusUnprocessableEntity,
				"Could not reach version on specified url %s: %s",
				ver.URL, err.Error())
			return
		}

		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}

		name := hdr.Name

		if split := strings.SplitN(name, "/", 2); len(split) == 2 {
			if prefix == "" {
				prefix = split[0]
			} else if prefix != split[0] {
				prefix = ""
			}
			name = split[1]
		}

		if name == manName {
			manifestContent, err = ioutil.ReadAll(tarReader)
			if err != nil {
				err = errshttp.NewError(http.StatusUnprocessableEntity,
					"Could not reach version on specified url %s: %s",
					ver.URL, err.Error())
				return
			}
		}
	}

	shasum, _ := hex.DecodeString(ver.Sha256)
	if !bytes.Equal(shasum, h.Sum(nil)) {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Checksum does not match the calculated one")
		return
	}

	if ver.Size > 0 && counter.Written() != ver.Size {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Size of the version does not match with the calculated one: expected %d and got %d",
			ver.Size, counter.Written())
		return
	}

	if len(ver.Signature) > 0 {
		if !editor.VerifySignature(shasum, ver.Signature) {
			err = errshttp.NewError(http.StatusUnprocessableEntity, "Bad signature")
			return
		}
	}

	if len(manifestContent) == 0 {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Application tarball does not contain a manifest")
		return
	}

	var manifest map[string]interface{}
	if err = json.Unmarshal(manifestContent, &manifest); err != nil {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Content of the manifest is not JSON valid: %s", err.Error())
		return
	}

	checkVals := map[string]interface{}{}
	checkVals["editor"] = app.Editor
	if ch := getVersionChannel(ver.Version); ch == Stable || ch == Beta {
		checkVals["version"] = ver.Version
	}

	if err = assertValues(manifest, checkVals); err != nil {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Content of the manifest does not match version object: %s",
			err.Error())
		return
	}

	size = counter.Written()
	return
}

func getVersionChannel(version string) Channel {
	if strings.Contains(version, devSuffix) {
		return Dev
	}
	if strings.Contains(version, betaSuffix) {
		return Beta
	}
	return Stable
}

func getManifestName(appType string) string {
	switch appType {
	case "webapp":
		return "manifest.webapp"
	case "konnector":
		return "manifest.konnector"
	}
	panic(fmt.Errorf("Uknown application type %s", appType))
}

func strToChannel(channel string) (Channel, error) {
	switch channel {
	case string(Stable):
		return Stable, nil
	case string(Beta):
		return Beta, nil
	case string(Dev):
		return Dev, nil
	default:
		return Stable, ErrChannelInvalid
	}
}
