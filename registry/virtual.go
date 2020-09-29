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
	"os"
	"path"
	"path/filepath"

	"github.com/cozy/cozy-apps-registry/asset"

	"github.com/cozy/cozy-apps-registry/space"

	"github.com/cozy/cozy-apps-registry/base"
	"github.com/go-kivik/kivik/v3"
)

func findOverwrittenVersion(s base.VirtualSpace, version *Version) (*Version, error) {
	db := s.VersionDB()
	ctx := context.Background()
	row := db.Get(ctx, version.ID)
	var t Version
	if err := row.ScanDoc(&t); err != nil {
		if kivik.StatusCode(err) == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

func DeleteOverwrittenVersion(s base.VirtualSpace, version *Version) error {
	overwritten, err := findOverwrittenVersion(s, version)
	if err != nil {
		return err
	}
	if overwritten == nil {
		return nil
	}
	if err := deleteOverwrittenTarball(s, overwritten); err != nil {
		return err
	}
	db := s.VersionDB()
	_, err = db.Delete(context.Background(), overwritten.ID, overwritten.Version)
	return err
}

func storeOverwrittenTarball(s base.VirtualSpace, tarball string) (hash string, length int64, err error) {
	file, err := os.Open(tarball)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		cerr := file.Close()
		if err == nil {
			err = cerr
		}
	}()

	stats, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	length = stats.Size()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", 0, err
	}
	h := hasher.Sum(nil)
	hash = hex.EncodeToString(h)

	prefix := base.Prefix(s.Name)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}

	if err = base.Storage.EnsureExists(prefix); err != nil {
		return "", 0, err
	}
	if err = base.Storage.Create(prefix, hash, "application/gzip", file); err != nil {
		return "", 0, err
	}

	return hash, length, nil
}

func deleteOverwrittenTarball(s base.VirtualSpace, version *Version) error {
	h, ok := version.AttachmentReferences["tarball"]
	if ok {
		prefix := base.Prefix(s.Name)
		if err := base.Storage.Remove(prefix, h); err != nil {
			return err
		}
	}
	return nil
}

func getOriginalTarball(space *space.Space, version *Version) (io.Reader, error) {
	url, err := url.Parse(version.URL)
	if err != nil {
		return nil, err
	}
	filename := filepath.Base(url.Path)

	att, err := FindVersionAttachment(space, version, filename)
	if err != nil {
		return nil, err
	}

	return att.Content, nil
}

func generateOverwrittenTarball(version *Version, overwrite map[string]interface{}, input io.Reader) (file string, manifest map[string]interface{}, icon string, err error) {
	var newManifest map[string]interface{}

	var originManifest Manifest
	if err := json.Unmarshal(version.Manifest, &originManifest); err != nil {
		return "", nil, "", err
	}

	iconFilename := originManifest.Icon
	manifestFilename := "manifest." + version.Type

	iconChecksum, iconOverwritten := overwrite["icon"].(string)
	var iconContent *bytes.Buffer
	if iconOverwritten {
		iconContent, _, err = base.GlobalAssetStore.Get(iconChecksum)
		if err != nil {
			return "", nil, "", err
		}
	}
	name, nameOverwritten := overwrite["name"].(string)

	inputGzip, err := gzip.NewReader(input)
	if err != nil {
		return "", nil, "", err
	}
	defer inputGzip.Close()
	inputTar := tar.NewReader(inputGzip)

	prefix := fmt.Sprintf("%s_%s_*.tar.gz", version.Slug, version.Version)
	outputFile, err := ioutil.TempFile("", prefix)
	if err != nil {
		return "", nil, "", err
	}
	file = outputFile.Name()
	// Tricky return case from here. Never `return nil, err`.
	// File is already created on filesystem, so we need to return it in all case to be able to clean it on the caller.
	// We can'm `defer os.Remove` here, because caller need the file for storage…

	outputGzip := gzip.NewWriter(outputFile)
	if err != nil {
		return "", nil, "", err
	}
	defer func() {
		cerr := outputGzip.Close()
		if err == nil {
			err = cerr
		}
	}()

	outputTar := tar.NewWriter(outputGzip)
	defer func() {
		cerr := outputTar.Close()
		if err == nil {
			err = cerr
		}
	}()

out:
	for {
		header, err := inputTar.Next()
		switch {
		case err == io.EOF:
			break out
		case err != nil:
			return file, nil, "", err
		case header == nil:
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err = outputTar.WriteHeader(header); err != nil {
				return file, nil, "", err
			}
		case tar.TypeReg:
			switch header.Name {
			case iconFilename:
				if iconOverwritten {
					header.Size = int64(iconContent.Len())
					if err = outputTar.WriteHeader(header); err != nil {
						return file, nil, "", err
					}
					if _, err := io.Copy(outputTar, iconContent); err != nil {
						return file, nil, "", err
					}
				} else {
					if err = outputTar.WriteHeader(header); err != nil {
						return file, nil, "", err
					}
					if _, err = io.Copy(outputTar, inputTar); err != nil {
						return file, nil, "", err
					}
				}
			case manifestFilename:
				if nameOverwritten {
					decoder := json.NewDecoder(inputTar)
					if err = decoder.Decode(&newManifest); err != nil {
						return file, nil, "", err
					}
					newManifest["name"] = name
					j, err := json.Marshal(newManifest)
					if err != nil {
						return file, nil, "", err
					}
					header.Size = int64(len(j))
					if err = outputTar.WriteHeader(header); err != nil {
						return file, nil, "", err
					}
					if _, err = outputTar.Write(j); err != nil {
						return file, nil, "", err
					}
				} else {
					if err = outputTar.WriteHeader(header); err != nil {
						return file, nil, "", err
					}
					if _, err = io.Copy(outputTar, inputTar); err != nil {
						return file, nil, "", err
					}
				}
			default:
				if err = outputTar.WriteHeader(header); err != nil {
					return file, nil, "", err
				}
				if _, err = io.Copy(outputTar, inputTar); err != nil {
					return file, nil, "", err
				}
			}
		}
	}

	return file, newManifest, icon, err
}

func versionOverwrittenAlreadyProcessed(versions []*Version, version *Version) bool {
	for _, curr := range versions {
		if version.Version == curr.Version {
			return true
		}
	}
	return false
}

func regenerateOverwrittenTarballs(virtualSpaceName string, appSlug string) (err error) {
	db, err := getDBForVirtualSpace(virtualSpaceName)
	if err != nil {
		return err
	}

	virtualSpace, ok := base.Config.VirtualSpaces[virtualSpaceName]
	if !ok {
		return fmt.Errorf("unable to find virtual space %s", virtualSpaceName)
	}

	spaceName := virtualSpace.Source
	s, ok := space.GetSpace(spaceName)
	if !ok {
		return fmt.Errorf("unable to find %s space", spaceName)
	}

	overwrite, err := findOverwrite(db, appSlug)
	if err != nil {
		return err
	}

	var regenerated []*Version

	for _, channel := range Channels {
		lastVersion, err := FindLatestVersion(s, appSlug, channel)
		if err != nil {
			return err
		}
		if lastVersion == nil || versionOverwrittenAlreadyProcessed(regenerated, lastVersion) {
			continue
		}

		tarball, err := getOriginalTarball(s, lastVersion)
		if err != nil {
			return err
		}
		file, manifest, icon, err := generateOverwrittenTarball(lastVersion, overwrite, tarball)
		// Tricky return, last part
		// Even in case of error, we must to take care of any file returned to be sure to erase it from the fs
		if file != "" {
			defer func() {
				cerr := os.Remove(file)
				if err == nil {
					err = cerr
				}
			}()
		}
		if err != nil {
			return err
		}

		hash, size, err := storeOverwrittenTarball(virtualSpace, file)
		if err != nil {
			return err
		}

		newVersion := lastVersion.Clone()
		newVersion.Rev = ""
		newVersion.AttachmentReferences = map[string]string{"tarball": hash}
		newVersion.Size = size
		newVersion.Sha256 = hash

		u, err := url.Parse(newVersion.URL)
		if err != nil {
			return err
		}
		u.Path = path.Join(virtualSpace.Name, u.Path)
		newVersion.URL = u.String()

		if icon != "" {
			newVersion.AttachmentReferences["icon"] = icon
		}
		j, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		newVersion.Manifest = j

		existingVersion, err := findOverwrittenVersion(virtualSpace, lastVersion)
		if err != nil {
			return err
		}
		if existingVersion != nil {
			newVersion.Rev = existingVersion.Rev
			// We already have a version, destroy the old tarball if changed
			h, ok := existingVersion.AttachmentReferences["tarball"]
			if ok && hash != h {
				prefix := base.Prefix(virtualSpace.Name)
				if err := base.Storage.Remove(prefix, h); err != nil {
					return err
				}
			}
		}

		db := virtualSpace.VersionDB()
		if _, err = db.Put(context.Background(), newVersion.ID, newVersion); err != nil {
			return err
		}

		regenerated = append(regenerated, lastVersion)
	}

	return nil
}

// FindAppOverride finds if the app have overwritten value in the virtual space
func FindAppOverride(virtualSpace *base.VirtualSpace, appSlug string, name string) (*string, error) {
	db, err := getDBForVirtualSpace(virtualSpace.Name)
	if err != nil {
		return nil, err
	}

	overwrite, err := findOverwrite(db, appSlug)
	if err != nil {
		return nil, err
	}

	value, ok := overwrite[name].(string)
	if !ok {
		return nil, nil
	}

	return &value, nil
}

// FindAttachmentFromOverwrite finds if the app was overwritten in the virtual space.
func FindAttachmentFromOverwrite(space *base.VirtualSpace, appSlug, filename string) (*Attachment, error) {
	shasum, err := FindAppOverride(space, appSlug, filename)
	if err != nil {
		return nil, err
	}
	if shasum == nil {
		return nil, nil
	}

	content, headers, err := base.GlobalAssetStore.Get(*shasum)
	if err != nil {
		return nil, err
	}

	return &Attachment{
		ContentType:   headers["Content-Type"],
		Content:       content,
		Etag:          headers["Etag"],
		ContentLength: headers["Content-Length"],
	}, nil
}

func FindOverwrittenVersion(space *base.VirtualSpace, version *Version) (*Version, error) {
	db := space.VersionDB()
	row := db.Get(context.Background(), version.ID)

	var doc Version
	err := row.ScanDoc(&doc)
	if err != nil {
		if kivik.StatusCode(err) == http.StatusNotFound {
			return nil, nil
		} else {
			return nil, err
		}
	}
	return &doc, nil
}

func FindOverwrittenTarball(space *base.VirtualSpace, version *Version) (*Attachment, error) {
	doc, err := FindOverwrittenVersion(space, version)
	if err != nil || doc == nil {
		return nil, err
	}
	checksum, ok := doc.AttachmentReferences["tarball"]
	if !ok {
		return nil, nil
	}

	prefix := base.Prefix(space.Name)
	content, headers, err := base.Storage.Get(prefix, checksum)
	if err != nil {
		return nil, err
	}

	return &Attachment{
		ContentType:   headers["Content-Type"],
		Content:       content,
		Etag:          headers["Etag"],
		ContentLength: headers["Content-Length"],
	}, nil
}

// OverwriteAppName tells that an app will have a different name in the virtual
// space.
func OverwriteAppName(virtualSpaceName, appSlug, newName string) error {
	db, err := getDBForVirtualSpace(virtualSpaceName)
	if err != nil {
		return err
	}

	overwrite, err := findOverwrite(db, appSlug)
	if err != nil {
		return err
	}
	overwrite["name"] = newName

	id := getAppID(appSlug)
	if _, err = db.Put(context.Background(), id, overwrite); err != nil {
		return err
	}

	if err := regenerateOverwrittenTarballs(virtualSpaceName, appSlug); err != nil {
		return err
	}

	return nil
}

// OverwriteAppIcon tells that an app will have a different icon in the virtual
// space.
func OverwriteAppIcon(virtualSpaceName, appSlug, file string) error {
	icon, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() {
		cerr := icon.Close()
		if err == nil {
			err = cerr
		}
	}()

	db, err := getDBForVirtualSpace(virtualSpaceName)
	if err != nil {
		return err
	}

	overwrite, err := findOverwrite(db, appSlug)
	if err != nil {
		return err
	}

	source := asset.ComputeSource(base.Prefix(virtualSpaceName), appSlug, "*")
	a := &base.Asset{
		Name:        filepath.Base(file),
		AppSlug:     appSlug,
		ContentType: getMIMEType(file, []byte{}),
	}
	if err = base.GlobalAssetStore.Add(a, icon, source); err != nil {
		return err
	}
	overwrite["icon"] = a.Shasum

	id := getAppID(appSlug)
	_, err = db.Put(context.Background(), id, overwrite)

	if err := regenerateOverwrittenTarballs(virtualSpaceName, appSlug); err != nil {
		return err
	}

	return err
}

// ActivateMaintenanceVirtualSpace tells that an app is in maintenance in the
// given virtual space.
func ActivateMaintenanceVirtualSpace(virtualSpaceName, appSlug string, opts MaintenanceOptions) error {
	db, err := getDBForVirtualSpace(virtualSpaceName)
	if err != nil {
		return err
	}

	overwrite, err := findOverwrite(db, appSlug)
	if err != nil {
		return err
	}
	overwrite["maintenance_activated"] = true
	overwrite["maintenance_options"] = opts

	id := getAppID(appSlug)
	_, err = db.Put(context.Background(), id, overwrite)
	return err
}

// DeactivateMaintenanceVirtualSpace tells that an app is no longer in
// maintenance in the given virtual space.
func DeactivateMaintenanceVirtualSpace(virtualSpaceName, appSlug string) error {
	db, err := getDBForVirtualSpace(virtualSpaceName)
	if err != nil {
		return err
	}

	overwrite, err := findOverwrite(db, appSlug)
	if err != nil {
		return err
	}
	delete(overwrite, "maintenance_activated")
	delete(overwrite, "maintenance_options")

	id := getAppID(appSlug)
	_, err = db.Put(context.Background(), id, overwrite)
	return err
}

func getDBForVirtualSpace(virtualSpaceName string) (*kivik.DB, error) {
	dbName := base.VirtualDBName(virtualSpaceName)
	ok, err := base.DBClient.DBExists(context.Background(), dbName)
	if err != nil {
		return nil, err
	}
	if !ok {
		fmt.Printf("Creating database %q...", dbName)
		if err = base.DBClient.CreateDB(context.Background(), dbName); err != nil {
			fmt.Println("failed")
			return nil, err
		}
		fmt.Println("ok.")
	}
	db := base.DBClient.DB(context.Background(), dbName)
	if err = db.Err(); err != nil {
		return nil, err
	}
	return db, nil
}

func findOverwrite(db *kivik.DB, appSlug string) (map[string]interface{}, error) {
	if !validSlugReg.MatchString(appSlug) {
		return nil, ErrAppSlugInvalid
	}

	doc := map[string]interface{}{}
	row := db.Get(context.Background(), getAppID(appSlug))
	err := row.ScanDoc(&doc)
	if err != nil && kivik.StatusCode(err) != http.StatusNotFound {
		return nil, err
	}

	return doc, nil
}

func FindOverwrite(virtualSpace *base.VirtualSpace, slug string) (map[string]interface{}, error) {
	db := virtualSpace.OverrideDb()
	return findOverwrite(db, slug)
}
