package scraper

import (
	"errors"

	"github.com/zogwine/metadata/internal/database"
	"github.com/zogwine/metadata/internal/status"
)

type ScraperScanConfig struct {
	AutoAdd            bool
	AddUnknown         bool
	Enable3DScan       bool
	MaxConcurrentScans int64
}

func StartScan(s *status.Status, mediaType database.MediaType, lib int64, conf ScraperScanConfig) error {

	switch mediaType {
	case database.MediaTypeTvs:
		if lib == 0 {
			return errors.New("library id is required for tvshow scan")
		}
		tv := NewTVSScraper(s)
		return tv.Scan(lib, conf)
	}

	return errors.New("unsupported media type")
}
