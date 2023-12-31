package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	fuzzy "github.com/paul-mannino/go-fuzzywuzzy"
	database "github.com/zogwine/metadata/internal/database"
	"github.com/zogwine/metadata/internal/scraper/common"
	"github.com/zogwine/metadata/internal/status"
	"github.com/zogwine/metadata/internal/util"
)

// scraper definition
type Scraper interface {
	Scan(idlib int64, conf ScraperScanConfig) error
	UpdateWithSelectionResult(mediaData int64, selection SelectionResult) error
}

type SelectionResult struct {
	ScraperName string
	ScraperID   string
	ScraperData string
}

// Select the best SearchData from an array based on a provided title and an optionnal year
// if no matching item is found or that the score is too low, an error is returned
func SelectBestItem(items []common.SearchData, title string, year int) (common.SearchData, error) {
	searchItems := []common.SearchData{}

	if year > 0 {
		for _, i := range items {
			if time.Unix(i.Premiered, 0).Year() == year {
				searchItems = append(searchItems, i)
			}
		}
	} else {
		searchItems = items
	}

	names := []string{}
	for _, i := range searchItems {
		names = append(names, i.Title)
	}

	match, err := fuzzy.ExtractOne(title, names)

	if err == nil && match.Score > 85 {
		return searchItems[util.Index(names, match.Match)], nil
	}

	return common.SearchData{}, errors.New("no data")
}

// Returns a list of the enabled scrapers and map of scraper name to config for a specific mediaType sorted by priority
func ListScraperConfiguration(s *status.Status, mediaType database.MediaType) ([]string, map[string](map[string]string), error) {
	ctx := context.Background()
	names := []string{}
	config := map[string](map[string]string){}

	data, err := s.DB.ListScraperForType(ctx, mediaType)
	if err != nil {
		return names, config, err
	}

	for _, i := range data {
		if i.Enabled {
			names = append(names, i.Provider)
			x := make(map[string]string)
			err = json.Unmarshal(i.Settings, &x)
			if err != nil {
				return names, config, err
			}
			config[i.Provider] = x
		}
	}

	return names, config, nil
}

// Add multiple results for a given mediaType/mediaData to the database
// also deletes the previous entries for the given mediaType/mediaData
func AddMultipleResults(s *status.Status, mediaType database.MediaType, mediaData int64, searchResults []common.SearchData, name string) error {
	ctx := context.Background()
	err := s.DB.DeleteMultipleResultsByMedia(ctx, database.DeleteMultipleResultsByMediaParams{MediaType: mediaType, MediaData: mediaData})
	if err != nil {
		return err
	}

	jsonData, err := json.Marshal(searchResults)
	if err != nil {
		return err
	}

	return s.DB.AddMultipleResults(ctx, database.AddMultipleResultsParams{MediaType: mediaType, MediaData: mediaData, Data: json.RawMessage(jsonData), Name: name})
}

// Select the result at index id for the given mediaType/mediaData
// and returns the selected SearchData
func SelectScraperResult(s *status.Status, mediaType database.MediaType, mediaData int64, id int) (common.SearchData, error) {
	ctx := context.Background()

	data, err := s.DB.GetMultipleResultsByMedia(ctx, database.GetMultipleResultsByMediaParams{MediaType: mediaType, MediaData: mediaData})
	if err != nil {
		return common.SearchData{}, err
	}

	searchData := []common.SearchData{}
	err = json.Unmarshal(data.Data, &searchData)
	if err != nil {
		return common.SearchData{}, err
	}

	sc, err := getScraperFromMediaType(s, mediaType)
	if err != nil {
		return common.SearchData{}, err
	}
	err = sc.UpdateWithSelectionResult(mediaData, SelectionResult{ScraperName: searchData[id].ScraperName, ScraperID: searchData[id].ScraperID, ScraperData: searchData[id].ScraperData})
	if err != nil {
		return common.SearchData{}, err
	}

	if len(searchData) < id {
		return common.SearchData{}, errors.New("invalid id")
	}

	err = s.DB.DeleteMultipleResultsByMedia(ctx, database.DeleteMultipleResultsByMediaParams{MediaType: mediaType, MediaData: mediaData})
	if err != nil {
		return common.SearchData{}, err
	}

	return searchData[id], nil
}

// Link a tag to a mediaType/mediaData in the database
// if the tag doesn't exists, it is automatically created
func AddTag(s *status.Status, mediaType database.MediaType, mediaData int64, tag common.TagData) error {
	ctx := context.Background()
	var tagID int64

	tagData, err := s.DB.GetTagByValue(ctx, database.GetTagByValueParams{Name: tag.Name, Value: tag.Value})
	if err != nil || tagData.Name == "" {
		// if tag does not exists, add it
		tagID, err = s.DB.AddTag(ctx, database.AddTagParams{Name: tag.Name, Value: tag.Value, Icon: tag.Icon})
		if err != nil {
			return err
		}
	} else {
		tagID = tagData.ID
	}

	// create link between tag and mediaType/mediaData
	return s.DB.AddTagLink(ctx, database.AddTagLinkParams{IDTag: tagID, MediaType: mediaType, MediaData: mediaData})
}

// Link a person to a mediaType/mediaData in the database
// if the person doesn't exists, it is automatically created
func AddPerson(s *status.Status, mediaType database.MediaType, mediaData int64, person common.PersonData) error {
	ctx := context.Background()
	var personID int64

	personData, err := s.DB.GetPersonByName(ctx, person.Name)
	if err != nil || personData.Name == "" {
		// if person does not exists, add it
		personID, err = s.DB.AddPerson(ctx, database.AddPersonParams{Name: person.Name})
		if err != nil {
			return err
		}
	} else {
		personID = personData.ID
	}

	// create link between person and mediaType/mediaData
	return s.DB.AddPersonLink(ctx, database.AddPersonLinkParams{IDPerson: personID, MediaType: mediaType, MediaData: mediaData})
}

func getScraperFromMediaType(s *status.Status, mediaType database.MediaType) (Scraper, error) {
	if mediaType == database.MediaTypeTvs {
		t := NewTVSScraper(s)
		return &t, nil
	} else {
		return nil, errors.New("no registered scraper for this mediatype")
	}
}
