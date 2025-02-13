package home

import (
	"fmt"
	"hash/crc32"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AdguardTeam/AdGuardHome/dnsfilter"
	"github.com/AdguardTeam/golibs/file"
	"github.com/AdguardTeam/golibs/log"
)

var (
	nextFilterID      = time.Now().Unix() // semi-stable way to generate an unique ID
	filterTitleRegexp = regexp.MustCompile(`^! Title: +(.*)$`)
)

// field ordering is important -- yaml fields will mirror ordering from here
type filter struct {
	Enabled     bool      `json:"enabled"`
	URL         string    `json:"url"`
	Name        string    `json:"name" yaml:"name"`
	RulesCount  int       `json:"rulesCount" yaml:"-"`
	LastUpdated time.Time `json:"lastUpdated,omitempty" yaml:"-"`
	checksum    uint32    // checksum of the file data

	dnsfilter.Filter `yaml:",inline"`
}

// Creates a helper object for working with the user rules
func userFilter() filter {
	f := filter{
		// User filter always has constant ID=0
		Enabled: true,
	}
	f.Filter.Data = []byte(strings.Join(config.UserRules, "\n"))
	return f
}

// Enable or disable a filter
func filterEnable(url string, enable bool) bool {
	r := false
	config.Lock()
	for i := range config.Filters {
		filter := &config.Filters[i] // otherwise we will be operating on a copy
		if filter.URL == url {
			filter.Enabled = enable
			if enable {
				e := filter.load()
				if e != nil {
					// This isn't a fatal error,
					//  because it may occur when someone removes the file from disk.
					// In this case the periodic update task will try to download the file.
					filter.LastUpdated = time.Time{}
					log.Tracef("%s filter load: %v", url, e)
				}
			} else {
				filter.unload()
			}
			r = true
			break
		}
	}
	config.Unlock()
	return r
}

// Return TRUE if a filter with this URL exists
func filterExists(url string) bool {
	r := false
	config.RLock()
	for i := range config.Filters {
		if config.Filters[i].URL == url {
			r = true
			break
		}
	}
	config.RUnlock()
	return r
}

// Add a filter
// Return FALSE if a filter with this URL exists
func filterAdd(f filter) bool {
	config.Lock()

	// Check for duplicates
	for i := range config.Filters {
		if config.Filters[i].URL == f.URL {
			config.Unlock()
			return false
		}
	}

	config.Filters = append(config.Filters, f)
	config.Unlock()
	return true
}

// Load filters from the disk
// And if any filter has zero ID, assign a new one
func loadFilters() {
	for i := range config.Filters {
		filter := &config.Filters[i] // otherwise we're operating on a copy
		if filter.ID == 0 {
			filter.ID = assignUniqueFilterID()
		}

		if !filter.Enabled {
			// No need to load a filter that is not enabled
			continue
		}

		err := filter.load()
		if err != nil {
			// This is okay for the first start, the filter will be loaded later
			log.Debug("Couldn't load filter %d contents due to %s", filter.ID, err)
		}
	}
}

func deduplicateFilters() {
	// Deduplicate filters
	i := 0 // output index, used for deletion later
	urls := map[string]bool{}
	for _, filter := range config.Filters {
		if _, ok := urls[filter.URL]; !ok {
			// we didn't see it before, keep it
			urls[filter.URL] = true // remember the URL
			config.Filters[i] = filter
			i++
		}
	}

	// all entries we want to keep are at front, delete the rest
	config.Filters = config.Filters[:i]
}

// Set the next filter ID to max(filter.ID) + 1
func updateUniqueFilterID(filters []filter) {
	for _, filter := range filters {
		if nextFilterID < filter.ID {
			nextFilterID = filter.ID + 1
		}
	}
}

func assignUniqueFilterID() int64 {
	value := nextFilterID
	nextFilterID++
	return value
}

// Sets up a timer that will be checking for filters updates periodically
func periodicallyRefreshFilters() {
	for range time.Tick(time.Minute) {
		refreshFiltersIfNecessary(false)
	}
}

// Checks filters updates if necessary
// If force is true, it ignores the filter.LastUpdated field value
//
// Algorithm:
// . Get the list of filters to be updated
// . For each filter run the download and checksum check operation
//  . If filter data hasn't changed, set new update time
//  . If filter data has changed, parse it, save it on disk, set new update time
//  . Apply changes to the current configuration
// . Restart server
func refreshFiltersIfNecessary(force bool) int {
	var updateFilters []filter

	if config.firstRun {
		return 0
	}

	config.RLock()
	for i := range config.Filters {
		f := &config.Filters[i] // otherwise we will be operating on a copy

		if !f.Enabled {
			continue
		}

		if !force && time.Since(f.LastUpdated) <= updatePeriod {
			continue
		}

		var uf filter
		uf.ID = f.ID
		uf.URL = f.URL
		uf.Name = f.Name
		uf.checksum = f.checksum
		updateFilters = append(updateFilters, uf)
	}
	config.RUnlock()

	updateCount := 0
	for i := range updateFilters {
		uf := &updateFilters[i]
		updated, err := uf.update()
		if err != nil {
			log.Printf("Failed to update filter %s: %s\n", uf.URL, err)
			continue
		}
		if updated {
			// Saving it to the filters dir now
			err = uf.save()
			if err != nil {
				log.Printf("Failed to save the updated filter %d: %s", uf.ID, err)
				continue
			}

		} else {
			mtime := time.Now()
			e := os.Chtimes(uf.Path(), mtime, mtime)
			if e != nil {
				log.Error("os.Chtimes(): %v", e)
			}
			uf.LastUpdated = mtime
		}

		config.Lock()
		for k := range config.Filters {
			f := &config.Filters[k]
			if f.ID != uf.ID || f.URL != uf.URL {
				continue
			}
			f.LastUpdated = uf.LastUpdated
			if !updated {
				continue
			}

			log.Info("Updated filter #%d.  Rules: %d -> %d",
				f.ID, f.RulesCount, uf.RulesCount)
			f.Name = uf.Name
			f.Data = uf.Data
			f.RulesCount = uf.RulesCount
			f.checksum = uf.checksum
			updateCount++
		}
		config.Unlock()
	}

	if updateCount > 0 && isRunning() {
		err := reconfigureDNSServer()
		if err != nil {
			msg := fmt.Sprintf("SHOULD NOT HAPPEN: cannot reconfigure DNS server with the new filters: %s", err)
			panic(msg)
		}
	}
	return updateCount
}

// A helper function that parses filter contents and returns a number of rules and a filter name (if there's any)
func parseFilterContents(contents []byte) (int, string) {
	lines := strings.Split(string(contents), "\n")
	rulesCount := 0
	name := ""
	seenTitle := false

	// Count lines in the filter
	for _, line := range lines {

		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if line[0] == '!' {
			m := filterTitleRegexp.FindAllStringSubmatch(line, -1)
			if len(m) > 0 && len(m[0]) >= 2 && !seenTitle {
				name = m[0][1]
				seenTitle = true
			}
		} else {
			rulesCount++
		}
	}

	return rulesCount, name
}

// Perform upgrade on a filter
func (filter *filter) update() (bool, error) {
	log.Tracef("Downloading update for filter %d from %s", filter.ID, filter.URL)

	resp, err := client.Get(filter.URL)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		log.Printf("Couldn't request filter from URL %s, skipping: %s", filter.URL, err)
		return false, err
	}

	if resp.StatusCode != 200 {
		log.Printf("Got status code %d from URL %s, skipping", resp.StatusCode, filter.URL)
		return false, fmt.Errorf("got status code != 200: %d", resp.StatusCode)
	}

	contentType := strings.ToLower(resp.Header.Get("content-type"))
	if !strings.HasPrefix(contentType, "text/plain") {
		log.Printf("Non-text response %s from %s, skipping", contentType, filter.URL)
		return false, fmt.Errorf("non-text response %s", contentType)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Couldn't fetch filter contents from URL %s, skipping: %s", filter.URL, err)
		return false, err
	}

	// Check if the filter has been really changed
	checksum := crc32.ChecksumIEEE(body)
	if filter.checksum == checksum {
		log.Tracef("Filter #%d at URL %s hasn't changed, not updating it", filter.ID, filter.URL)
		return false, nil
	}

	// Extract filter name and count number of rules
	rulesCount, filterName := parseFilterContents(body)
	log.Printf("Filter %d has been updated: %d bytes, %d rules", filter.ID, len(body), rulesCount)
	if filterName != "" {
		filter.Name = filterName
	}
	filter.RulesCount = rulesCount
	filter.Data = body
	filter.checksum = checksum

	return true, nil
}

// saves filter contents to the file in dataDir
func (filter *filter) save() error {
	filterFilePath := filter.Path()
	log.Printf("Saving filter %d contents to: %s", filter.ID, filterFilePath)

	err := file.SafeWrite(filterFilePath, filter.Data)

	// update LastUpdated field after saving the file
	filter.LastUpdated = filter.LastTimeUpdated()
	return err
}

// loads filter contents from the file in dataDir
func (filter *filter) load() error {
	filterFilePath := filter.Path()
	log.Tracef("Loading filter %d contents to: %s", filter.ID, filterFilePath)

	if _, err := os.Stat(filterFilePath); os.IsNotExist(err) {
		// do nothing, file doesn't exist
		return err
	}

	filterFileContents, err := ioutil.ReadFile(filterFilePath)
	if err != nil {
		return err
	}

	log.Tracef("File %s, id %d, length %d", filterFilePath, filter.ID, len(filterFileContents))
	rulesCount, _ := parseFilterContents(filterFileContents)

	filter.RulesCount = rulesCount
	filter.Data = filterFileContents
	filter.checksum = crc32.ChecksumIEEE(filterFileContents)
	filter.LastUpdated = filter.LastTimeUpdated()

	return nil
}

// Clear filter rules
func (filter *filter) unload() {
	filter.Data = nil
	filter.RulesCount = 0
}

// Path to the filter contents
func (filter *filter) Path() string {
	return filepath.Join(config.ourWorkingDir, dataDir, filterDir, strconv.FormatInt(filter.ID, 10)+".txt")
}

// LastTimeUpdated returns the time when the filter was last time updated
func (filter *filter) LastTimeUpdated() time.Time {
	filterFilePath := filter.Path()
	s, err := os.Stat(filterFilePath)
	if os.IsNotExist(err) {
		// if the filter file does not exist, return 0001-01-01
		return time.Time{}
	}

	if err != nil {
		// if the filter file does not exist, return 0001-01-01
		return time.Time{}
	}

	// filter file modified time
	return s.ModTime()
}
