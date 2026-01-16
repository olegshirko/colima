package downloader

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/abiosoft/colima/config"
	"github.com/abiosoft/colima/environment"
	"github.com/abiosoft/colima/util/shautil"
	"github.com/abiosoft/colima/util/terminal"
	"github.com/sirupsen/logrus"
)

type (
	hostActions  = environment.HostActions
	guestActions = environment.GuestActions
)

// Request is download request
type Request struct {
	URL string // request URL
	SHA *SHA   // shasum url
}

// DownloadToGuest downloads file at url and saves it in the destination.
//
// In the implementation, the file is downloaded (and cached) on the host, but copied to the desired
// destination for the guest.
// filename must be an absolute path and a directory on the guest that does not require root access.
func DownloadToGuest(host hostActions, guest guestActions, log *logrus.Logger, r Request, filename string) error {
	// if file is on the filesystem, no need for download. A copy suffices
	if strings.HasPrefix(r.URL, "/") {
		return guest.RunQuiet("cp", r.URL, filename)
	}

	cacheFile, err := Download(host, log, r)
	if err != nil {
		return err
	}

	return guest.RunQuiet("cp", cacheFile, filename)
}

// Download downloads file at url and returns the location of the downloaded file.
func Download(host hostActions, log *logrus.Logger, r Request) (string, error) {
	d := downloader{
		host: host,
		log:  log,
	}

	if !d.hasCache(r.URL) {
		if err := d.downloadFile(r); err != nil {
			return "", fmt.Errorf("error downloading '%s': %w", r.URL, err)
		}
	}

	return CacheFilename(r.URL), nil
}

type downloader struct {
	host hostActions
	log  *logrus.Logger
}

// CacheFilename returns the computed filename for the url.
func CacheFilename(url string) string {
	return filepath.Join(config.CacheDir(), "caches", shautil.SHA256(url).String())
}

func (d downloader) cacheDownloadingFileName(url string) string {
	return CacheFilename(url) + ".downloading"
}

func (d downloader) downloadFile(r Request) (err error) {
	d.log.Tracef("downloading %s", r.URL)

	// save to a temporary file initially before renaming to the desired file after successful download
	// this prevents having a corrupt file
	cacheDownloadingFilename := d.cacheDownloadingFileName(r.URL)
	if err := os.MkdirAll(filepath.Dir(cacheDownloadingFilename), 0755); err != nil {
		err = fmt.Errorf("error preparing cache dir: %w", err)
		d.log.Tracef("error downloading %s: %v", r.URL, err)
		return err
	}

	// create file, or open if it exists
	destFile, err := os.OpenFile(cacheDownloadingFilename, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		err = fmt.Errorf("error creating destination file: %w", err)
		d.log.Tracef("error downloading %s: %v", r.URL, err)
		return err
	}
	defer destFile.Close()

	// check file size to resume download
	stat, err := destFile.Stat()
	if err != nil {
		err = fmt.Errorf("error getting file stat: %w", err)
		d.log.Tracef("error downloading %s: %v", r.URL, err)
		return err
	}
	currentSize := stat.Size()

	req, err := http.NewRequest("GET", r.URL, nil)
	if err != nil {
		err = fmt.Errorf("error creating request: %w", err)
		d.log.Tracef("error downloading %s: %v", r.URL, err)
		return err
	}
	if currentSize > 0 {
		d.log.Tracef("resuming download from byte %d", currentSize)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", currentSize))
	}

	// custom transport to avoid timeout on slow connections
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("error during download: %w", err)
		d.log.Tracef("error downloading %s: %v", r.URL, err)
		return err
	}
	defer resp.Body.Close()

	// if server does not support resume, start from scratch
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		if err := destFile.Truncate(0); err != nil {
			err = fmt.Errorf("error truncating file: %w", err)
			d.log.Tracef("error downloading %s: %v", r.URL, err)
			return err
		}
		// reset current size
		currentSize = 0
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		d.log.Tracef("error downloading %s: %v", r.URL, err)
		return err
	}

	// seek to the end of file for appending if resumable, otherwise start from scratch
	if resp.StatusCode == http.StatusPartialContent {
		if _, err := destFile.Seek(0, io.SeekEnd); err != nil {
			err = fmt.Errorf("error seeking to end of file: %w", err)
			d.log.Tracef("error downloading %s: %v", r.URL, err)
			return err
		}
	} else {
		if _, err := destFile.Seek(0, io.SeekStart); err != nil {
			err = fmt.Errorf("error seeking to start of file: %w", err)
			d.log.Tracef("error downloading %s: %v", r.URL, err)
			return err
		}
	}

	// copy stream to file
	progress := newProgress(d.log, resp.ContentLength+currentSize, currentSize)
	reader := io.TeeReader(resp.Body, progress)

	if _, err := io.Copy(destFile, reader); err != nil {
		err = fmt.Errorf("error writing to file: %w", err)
		d.log.Tracef("error downloading %s: %v", r.URL, err)
		return err
	}

	// validate download if sha is present
	if r.SHA != nil {
		if err := r.SHA.validateDownload(d.host, r.URL, cacheDownloadingFilename); err != nil {
			// move file to allow subsequent re-download
			// error discarded, would not be actioned anyways
			_ = os.Rename(cacheDownloadingFilename, cacheDownloadingFilename+".invalid")
			err = fmt.Errorf("error validating SHA sum for '%s': %w", path.Base(r.URL), err)
			d.log.Tracef("error downloading %s: %v", r.URL, err)
			return err
		}
	}

	d.log.Tracef("downloaded %s", r.URL)
	return os.Rename(cacheDownloadingFilename, CacheFilename(r.URL))
}

// Progress tracks download progress.
type Progress struct {
	Total      int64 // total size
	Current    int64 // downloaded size
	mu         sync.Mutex
	lastReport time.Time
	logger     *logrus.Logger
}

// newProgress creates a new Progress.
func newProgress(logger *logrus.Logger, total, current int64) *Progress {
	return &Progress{
		Total:   total,
		Current: current,
		logger:  logger,
	}
}

// Write implements io.Writer.
func (p *Progress) Write(b []byte) (int, error) {
	n := len(b)
	p.mu.Lock()
	defer p.mu.Unlock()

	p.Current += int64(n)

	// efficient not to report on every write
	if time.Since(p.lastReport) < (time.Second / 2) {
		return n, nil
	}

	p.lastReport = time.Now()
	// no new line
	fmt.Printf("\rdownloading ... %s ", terminal.Progress(p.Current, p.Total))
	return n, nil
}

func (d downloader) hasCache(url string) bool {
	_, err := os.Stat(CacheFilename(url))
	return err == nil
}
