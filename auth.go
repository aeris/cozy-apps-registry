package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/flimzy/kivik"
)

const tokenName = "editor-token"

type EditorTokenOptions struct {
	Editor string
	MaxAge time.Duration
}

type EditorRegistry interface {
	GetEditorSecret(editorName string) ([]byte, error)
	CreateEditorSecret(editorName string) error
}

func GenerateEditorToken(reg EditorRegistry, opts *EditorTokenOptions) ([]byte, error) {
	secret, err := reg.GetEditorSecret(opts.Editor)
	if err != nil {
		return nil, err
	}
	msg := []byte(opts.Editor)
	return EncodeAuthMessage(tokenConfig(secret), opts.MaxAge, msg)
}

func VerifyEditorToken(reg EditorRegistry, editorName, appName string, token []byte) error {
	secret, err := reg.GetEditorSecret(editorName)
	if err != nil {
		return err
	}
	msg, err := DecodeAuthMessage(tokenConfig(secret), token)
	if err != nil {
		return err
	}
	msgSplit := strings.SplitN(string(msg), ":", 2)
	if len(msgSplit) != 2 {
		return errUnauthorized
	}
	if msgSplit[0] != editorName {
		return errUnauthorized
	}
	if msgSplit[1] != "*" && !strings.HasPrefix(appName, msgSplit[1]) {
		return errUnauthorized
	}
	return nil
}

func tokenConfig(secret []byte) *MACConfig {
	return &MACConfig{
		Name:   tokenName,
		Key:    secret,
		MaxLen: 256,
	}
}

type couchdbEditorReg struct {
	db *kivik.DB
}

func NewCouchdbEditorRegistry(addr string) (EditorRegistry, error) {
	db, err := client.DB(ctx, editorsDB)
	if err != nil {
		return nil, err
	}
	return &couchdbEditorReg{db}, nil
}

func (r *couchdbEditorReg) GetEditorSecret(editorName string) ([]byte, error) {
	req := sprintfJSON(`
{
	"selector": { "name": %s },
	"limit": 1
}`, editorName)

	rows, err := r.db.Find(ctx, req)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, errUnknownEditor
	}
	var doc *Editor
	if err = rows.ScanDoc(&doc); err != nil {
		return nil, err
	}
	return doc.Secret, nil
}

func (r *couchdbEditorReg) CreateEditorSecret(editorName string) error {
	doc := &Editor{
		Name:   editorName,
		Secret: generateRandomBytes(32),
	}
	_, _, err := r.db.CreateDoc(ctx, doc)
	return err
}

type fileEditorReg struct {
	filename string
}

func NewFileEditorRegistry(filename string) (EditorRegistry, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		b := s.Text()
		if len(b) == 0 || b[0] == '#' {
			continue
		}
		fields := strings.Fields(b)
		if len(fields) < 2 {
			return nil, fmt.Errorf("Editor registry in file %s: format of each line should be: \"editorname secret-in-hex\"",
				filename)
		}
		token, err := hex.DecodeString(fields[1])
		if err != nil {
			return nil, fmt.Errorf("Editor registry in file %s: format of each line should be: \"editorname secret-in-hex\": %s",
				filename, err.Error())
		}
		if len(token) < 16 {
			return nil, fmt.Errorf("Editor registry in file %s: editor secret should contain at least 16 bytes",
				filename)
		}
	}
	return &fileEditorReg{filename}, nil
}

func (r *fileEditorReg) GetEditorSecret(editorName string) ([]byte, error) {
	f, err := os.Open(r.filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		b := s.Text()
		if len(b) == 0 || b[0] == '#' {
			continue
		}
		fields := strings.Fields(b)
		if len(fields) < 2 || fields[0] != editorName {
			continue
		}
		return hex.DecodeString(fields[1])
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return nil, errUnknownEditor
}

func (r *fileEditorReg) CreateEditorSecret(editorName string) error {
	_, err := r.GetEditorSecret(editorName)
	if err == nil {
		return errEditorExists
	}
	if err != errUnknownEditor {
		return err
	}
	f, err := os.OpenFile(r.filename, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0660)
	if err != nil {
		return err
	}
	secret := hex.EncodeToString(generateRandomBytes(32))
	s := fmt.Sprintf("%s\t%s\n", editorName, secret)
	if n, err := f.WriteString(s); err != nil || n != len(s) {
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	return nil
}
