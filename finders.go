package main

import "sort"

func FindApp(appName string) (*App, error) {
	if !validAppNameReg.MatchString(appName) {
		return nil, errBadAppName
	}
	db, err := client.DB(ctx, appsDB)
	if err != nil {
		return nil, err
	}
	req := sprintfJSON(`
{
  "selector": { "name": %s },
  "limit": 1
}`, appName)

	rows, err := db.Find(ctx, req)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, errAppNotFound
	}
	var doc *App
	if err = rows.ScanDoc(&doc); err != nil {
		return nil, err
	}

	doc.Versions, err = FindAppVersions(appName)
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func FindVersion(appName, version string) (*Version, error) {
	if !validAppNameReg.MatchString(appName) {
		return nil, errBadAppName
	}
	if !validVersionReg.MatchString(version) {
		return nil, errBadVersion
	}
	db, err := client.DB(ctx, versDB)
	if err != nil {
		return nil, err
	}

	req := sprintfJSON(`
{
  "selector": { "name": %s, "version": %s },
  "limit": 1
}`, appName, version)

	rows, err := db.Find(ctx, req)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, errVersionNotFound
	}
	var doc *Version
	if err := rows.ScanDoc(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func FindLatestVersion(appName string, channel string) (*Version, error) {
	ch, err := strToChannel(channel)
	if err != nil {
		return nil, err
	}
	if !validAppNameReg.MatchString(appName) {
		return nil, errBadAppName
	}
	db, err := client.DB(ctx, versDB)
	if err != nil {
		return nil, err
	}

	var latest *Version
	req := sprintfJSON(`
{
  "selector": { "name": %s },
  "limit": 2000
}`, appName)

	rows, err := db.Find(ctx, req)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var doc *Version
		if err = rows.ScanDoc(&doc); err != nil {
			return nil, err
		}
		switch ch {
		case Stable:
			if c := getVersionChannel(doc.Version); c != Stable {
				continue
			}
		case Beta:
			if c := getVersionChannel(doc.Version); c != Stable && c != Beta {
				continue
			}
		}
		if latest == nil || isVersionLess(latest, doc) {
			latest = doc
		}
	}
	if latest == nil {
		return nil, errVersionNotFound
	}
	return latest, nil
}

func FindAppVersions(appName string) (*AppVersions, error) {
	db, err := client.DB(ctx, versDB)
	if err != nil {
		return nil, err
	}

	var allVersions versionsSlice

	req := sprintfJSON(`
{
  "selector": { "name": %s },
  "fields": ["version", "created_at"],
  "limit": 2000
}`, appName)

	rows, err := db.Find(ctx, req)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var doc *Version
		if err = rows.ScanDoc(&doc); err != nil {
			return nil, err
		}
		allVersions = append(allVersions, doc)
	}
	sort.Sort(allVersions)

	stable := make([]string, 0)
	beta := make([]string, 0)
	dev := make([]string, 0)

	for _, v := range allVersions {
		switch getVersionChannel(v.Version) {
		case Stable:
			stable = append(stable, v.Version)
			fallthrough
		case Beta:
			beta = append(beta, v.Version)
			fallthrough
		case Dev:
			dev = append(dev, v.Version)
		}
	}

	return &AppVersions{
		Stable: stable,
		Beta:   beta,
		Dev:    dev,
	}, nil
}

func GetAppsList(filters map[string]string) ([]*App, error) {
	db, err := client.DB(ctx, appsDB)
	if err != nil {
		return nil, err
	}

	req := sprintfJSON(`
{
	"selector": {},
	"sort": [{ "name": "desc" }]
}`)
	rows, err := db.Find(ctx, req)
	if err != nil {
		return nil, err
	}
	var res []*App
	for rows.Next() {
		var doc *App
		if err = rows.ScanDoc(&doc); err != nil {
			return nil, err
		}
		res = append(res, doc)
	}
	return res, nil
}
