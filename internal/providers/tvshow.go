package scraper

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/zogwine/metadata/internal/database"
	"github.com/zogwine/metadata/internal/file"
	"github.com/zogwine/metadata/internal/scraper/common"
	"github.com/zogwine/metadata/internal/status"
	"github.com/zogwine/metadata/internal/util"
	"golang.org/x/sync/semaphore"
)

type TVSScraper struct {
	MediaType     database.MediaType
	IDLib         int64
	LibPath       string
	AutoAdd       bool
	AddUnknown    bool
	App           *status.Status
	Providers     map[string]common.TVShowProvider
	ProviderNames []string // list used to keep the order of preferences
	RegexSeason   *regexp.Regexp
	RegexEpisode  *regexp.Regexp
}

func (t *TVSScraper) getProviderFromName(pname string) (common.TVShowProvider, error) {
	for name, prov := range t.Providers {
		if name == pname {
			return prov, nil
		}
	}
	return nil, errors.New("provider " + pname + " not found")
}

func (t *TVSScraper) loadTVSPlugins() error {
	names, config, err := ListScraperConfiguration(t.App, database.MediaTypeTvs)

	if err != nil {
		return err
	}

	for _, i := range names {
		pl, err := util.LoadPlugin("TVShowProvider", "./plugins/scraper/"+i)
		if err == nil {
			p, ok := pl.(func() common.TVShowProvider)
			if ok {
				t.Providers[i] = p()
				t.Providers[i].Setup(config[i], t.App.Log)
				t.ProviderNames = append(t.ProviderNames, i)
			}
		}
	}

	t.App.Log.WithFields(log.Fields{"entity": "scraper", "file": "tvshow", "function": "loadTVSPlugins"}).Info("loaded providers: " + strings.Join(t.ProviderNames, ","))

	if len(t.Providers) == 0 {
		return errors.New("no provider loaded")
	}

	return nil
}

func NewTVSScraper(s *status.Status) TVSScraper {
	seasonReg := regexp.MustCompile(`(?i)(?:s)(\d+)(?:e)`)
	epReg := regexp.MustCompile(`(?i)(?:s\d+e)(\d+)`)
	t := TVSScraper{MediaType: database.MediaTypeTvs, IDLib: 0, AutoAdd: false, AddUnknown: true, App: s, Providers: map[string]common.TVShowProvider{}, ProviderNames: []string{}, RegexSeason: seasonReg, RegexEpisode: epReg}
	err := t.loadTVSPlugins()
	if err != nil {
		t.App.Log.WithFields(log.Fields{"entity": "scraper", "file": "tvshow", "function": "NewTVSScraper"}).Warn(err)
	}
	return t
}

func (t *TVSScraper) Scan(idlib int64, conf ScraperScanConfig) error {
	t.IDLib = idlib
	t.AutoAdd = conf.AutoAdd
	t.AddUnknown = conf.AddUnknown
	ctx := context.Background()

	// get library base path
	lib, err := t.App.DB.GetLibrary(ctx, t.IDLib)
	if err != nil {
		return errors.New("unable to retreive library path: " + err.Error())
	}
	t.LibPath = lib.Path

	// get data for existing tvs
	tvsData, err := t.App.DB.ListShow(ctx, 0)
	if err != nil {
		return err
	}

	tvsPaths := []string{}
	for _, i := range tvsData {
		tvsPaths = append(tvsPaths, i.Path)
	}

	// list items at this path
	items, err := os.ReadDir(t.LibPath)
	if err != nil {
		return err
	}

	if conf.MaxConcurrentScans < 2 {
		for _, i := range items {
			t.processItemScan(i, tvsPaths, tvsData)
		}
	} else {
		// TODO: fix bug with scaper when running a lot of concurrent goroutines (ex: 10)
		sem := semaphore.NewWeighted(conf.MaxConcurrentScans) // semaphore used to limit the number of concurrent goroutines running
		var wg sync.WaitGroup

		for _, i := range items {
			wg.Add(1)
			sem.Acquire(context.Background(), 1)
			go func(i fs.DirEntry, tvsPaths []string, tvsData []database.ListShowRow) {
				defer wg.Done()
				t.processItemScan(i, tvsPaths, tvsData)
				sem.Release(1)
			}(i, tvsPaths, tvsData)
		}
		wg.Wait()
	}

	return nil
}

// process each folder found at the root of our library, i.e. the tv shows
func (t *TVSScraper) processItemScan(i fs.DirEntry, tvsPaths []string, tvsData []database.ListShowRow) {
	var err error

	if i.IsDir() {
		// keep only the folders
		currentShow := util.Index(tvsPaths, i.Name())
		logF := log.Fields{"entity": "scraper", "file": "tvshow", "function": "Scan", "tvs": i.Name()}
		t.App.Log.WithFields(logF).Debugf("processing tvs: %q", i.Name())

		data := database.ListShowRow{}
		if currentShow > -1 {
			// if there is already an entry for this tvs
			t.App.Log.WithFields(logF).Trace("tvs already in database")
			data = tvsData[currentShow]
			if data.UpdateMode > 0 {
				// if updates are allowed
				if data.ScraperID == "" || data.ScraperName == "" || data.ScraperName == " " {
					// if no scraper is associated to this tvs, just re-run a search
					t.App.Log.WithFields(logF).Trace("add tvs")
					data, err = t.addTVS(data)
				} else {
					// else, update tvs metadata
					t.App.Log.WithFields(logF).Trace("update tvs")
					data, err = t.updateTVS(data)
				}
			} else {
				t.App.Log.WithFields(logF).Trace("no update needed")
			}
		} else {
			// if this is a newly discovered tvs
			t.App.Log.WithFields(logF).Trace("new tvs: " + i.Name())
			data.Title = i.Name()
			data, err = t.addTVS(data)
		}

		if err == nil && data.ScraperID != "" {
			// if a scraper is associated, update episodes
			t.App.Log.WithFields(logF).Trace("update episodes")
			err = t.updateTVSEpisodes(data)
		}

		if err != nil {
			t.App.Log.WithFields(logF).Error(err)
		}
	}
}

func (t *TVSScraper) addTVS(data database.ListShowRow) (database.ListShowRow, error) {
	logF := log.Fields{"entity": "scraper", "file": "tvshow", "function": "addTVS", "tvs": data.Title}
	searchResults := []common.SearchData{}
	var err error

	// retreive search results for each provider
	for _, i := range t.ProviderNames {
		res, err := t.Providers[i].SearchTVS(data.Title)
		if err == nil {
			searchResults = append(searchResults, res...)
		}
	}

	if len(searchResults) == 0 && !t.AddUnknown {
		return data, errors.New("no data avaiable for show " + data.Title)
	}

	if data.ID == 0 {
		// if this is a new tvs
		// create a new entry in the database
		data.ID, err = t.App.DB.AddShow(context.Background(), database.AddShowParams{
			Title:   data.Title,
			IDLib:   t.IDLib,
			AddDate: time.Now().Unix(),
		})
		if err != nil {
			return data, err
		}
	}

	if t.AutoAdd {
		// if we want to try to automatically select the best result
		selected, err := SelectBestItem(searchResults, data.Title, 0)
		if err == nil {
			t.App.Log.WithFields(logF).Tracef("auto select: %s: %s", selected.ScraperName, selected.ScraperID)
			// if a result was selected
			t.UpdateWithSelectionResult(data.ID, SelectionResult{ScraperName: selected.ScraperName, ScraperID: selected.ScraperID, ScraperData: selected.ScraperData})
			data.ScraperID = selected.ScraperID
			data.ScraperName = selected.ScraperName
			data.ScraperData = selected.ScraperData
			data.Path = data.Title
			// force tvs update
			return t.updateTVS(data)
		} else {
			t.App.Log.WithFields(logF).Trace("auto select failed, add multiple results")
			AddMultipleResults(t.App, database.MediaTypeTvs, data.ID, searchResults, data.Title)
		}
	} else {
		t.App.Log.WithFields(logF).Trace("add multiple results")
		AddMultipleResults(t.App, database.MediaTypeTvs, data.ID, searchResults, data.Title)
	}

	return data, nil
}

// update tvs, tags and people metadata
func (t *TVSScraper) updateTVS(data database.ListShowRow) (database.ListShowRow, error) {
	ctx := context.Background()
	provider, err := t.getProviderFromName(data.ScraperName)
	if err != nil {
		return data, err
	}
	provider.Configure(data.ScraperID, data.ScraperData)

	// update tvs metadata
	tvsData, err := provider.GetTVS()
	if err != nil {
		return data, err
	}
	err = t.App.DB.UpdateShow(ctx, database.UpdateShowParams{
		Title:       tvsData.Title,
		Overview:    tvsData.Overview,
		Icon:        tvsData.Icon,
		Fanart:      tvsData.Fanart,
		Website:     tvsData.Website,
		Trailer:     tvsData.Trailer,
		Premiered:   tvsData.Premiered,
		Rating:      tvsData.Rating,
		ScraperLink: tvsData.ScraperInfo.ScraperLink,
		ScraperData: tvsData.ScraperInfo.ScraperData,
		UpdateDate:  time.Now().Unix(),
		ID:          data.ID,
		UpdateMode:  -1,
	})
	if err != nil {
		return data, err
	}
	data.Title = tvsData.Title
	data.ScraperData = tvsData.ScraperInfo.ScraperData
	data.Premiered = tvsData.Premiered

	// update tags
	tagData, err := provider.ListTVSTag()
	if err != nil {
		return data, err
	}
	for _, i := range tagData {
		AddTag(t.App, database.MediaTypeTvs, data.ID, i)
	}

	// update people
	persData, err := provider.ListTVSPerson()
	if err != nil {
		return data, err
	}
	for _, i := range persData {
		AddPerson(t.App, database.MediaTypeTvs, data.ID, i)
	}

	return data, nil
}

// update tvs seasons and episodes metadata
func (t *TVSScraper) updateTVSEpisodes(data database.ListShowRow) error {
	logF := log.Fields{"entity": "scraper", "file": "tvshow", "function": "updateTVSEpisodes", "tvs": data.Title}

	// get path to the root tvs folder
	tvsPath := filepath.Join(t.LibPath, data.Path)
	t.App.Log.WithFields(logF).Tracef("processing episodes in: %s", tvsPath)

	// get provider
	provider, err := t.getProviderFromName(data.ScraperName)
	if err != nil {
		return err
	}
	provider.Configure(data.ScraperID, data.ScraperData)

	// list and update existing seasons
	seasons, err := t.updateTVSSeasons(provider, data.ID)
	if err != nil {
		return err
	}

	// for each file in the tvs folder
	for _, i := range ListFiles(tvsPath, true) {
		t.App.Log.WithFields(logF).Tracef("processing episode: %s", i)
		p := filepath.Join(data.Path, i)
		if file.IsVideo(t.App, p) {
			t.updateTVSEpisode(provider, &seasons, p, data.ID)
		}
	}

	return nil
}

// update existing seasons for a tvshow, returns the list of existing season numbers
func (t *TVSScraper) updateTVSSeasons(provider common.TVShowProvider, idshow int64) ([]int64, error) {
	ctx := context.Background()
	seasonData, err := t.App.DB.ListShowSeason(ctx, database.ListShowSeasonParams{IDUser: 0, IDShow: idshow})
	if err != nil {
		return []int64{}, err
	}
	seasons := []int64{}
	for _, i := range seasonData {
		if i.UpdateMode > 0 {
			// update the seasons if needed
			seasonData, err := provider.GetTVSSeason(int(i.Season))
			if err == nil {
				t.App.DB.UpdateShowSeason(ctx, database.UpdateShowSeasonParams{
					Title:       seasonData.Title,
					Overview:    seasonData.Overview,
					Icon:        seasonData.Icon,
					Season:      i.Season,
					Fanart:      seasonData.Fanart,
					Premiered:   seasonData.Premiered,
					Rating:      seasonData.Rating,
					Trailer:     seasonData.Trailer,
					ScraperName: seasonData.ScraperInfo.ScraperName,
					ScraperData: seasonData.ScraperInfo.ScraperData,
					ScraperID:   seasonData.ScraperInfo.ScraperID,
					ScraperLink: seasonData.ScraperInfo.ScraperLink,
					UpdateDate:  time.Now().Unix(),
					UpdateMode:  -1,
					IDShow:      idshow,
				})
			} else {
				t.App.Log.WithFields(log.Fields{"entity": "scraper", "file": "tvshow", "function": "updateTVSSeasons", "tvs": idshow}).Error(err)
			}
		}

		seasons = append(seasons, i.Season)
	}
	return seasons, nil
}

// update a tvshow episode based on the provided file path and idshow
// takes a seasons argument with a pointer to a list of the existing seasons for this show, this list will be modified if a new season is added
func (t *TVSScraper) updateTVSEpisode(provider common.TVShowProvider, seasons *[]int64, p string, idshow int64) {
	ctx := context.Background()
	filename := path.Base(p)
	logF := log.Fields{"entity": "scraper", "file": "tvshow", "function": "updateTVSEpisode", "tvs": filename}

	videoData, err := t.App.DB.GetVideoFileFromPath(ctx, database.GetVideoFileFromPathParams{IDLib: t.IDLib, Path: p})
	if err == nil {
		t.App.Log.WithFields(logF).Trace("update episode")

		episodeData, err := t.App.DB.GetShowEpisode(ctx, database.GetShowEpisodeParams{IDUser: 0, ID: videoData.MediaData})
		if err == nil && episodeData.UpdateMode > 0 {
			err = file.UpdateVideoFile(t.App, t.IDLib, p)
			epData, err := provider.GetTVSEpisode(int(episodeData.Season), int(episodeData.Episode))
			if err == nil {
				t.App.DB.UpdateShowEpisode(ctx, database.UpdateShowEpisodeParams{
					Title:       epData.Title,
					Overview:    epData.Overview,
					Icon:        epData.Icon,
					Premiered:   epData.Premiered,
					Rating:      epData.Rating,
					ScraperID:   epData.ScraperInfo.ScraperID,
					ScraperName: epData.ScraperInfo.ScraperName,
					ScraperData: epData.ScraperInfo.ScraperData,
					ScraperLink: epData.ScraperInfo.ScraperLink,
					UpdateDate:  time.Now().Unix(),
					UpdateMode:  -1,
					ID:          episodeData.ID,
				})
			} else {
				t.App.Log.WithFields(logF).Error(err)
			}
		} else {
			t.App.Log.WithFields(logF).Tracef("no update requested or error: %s", err)
		}
	} else {
		t.App.Log.WithFields(logF).Trace("no existing entry for this episode")
		// if there are no existing entries for this episodes

		// extract season and episode number from filename
		searchSeason := t.RegexSeason.FindStringSubmatch(filename)
		searchEpisode := t.RegexEpisode.FindStringSubmatch(filename)

		if len(searchSeason) > 1 && searchSeason[1] != "" && len(searchEpisode) > 1 && searchEpisode[1] != "" {
			season, _ := strconv.Atoi(string(searchSeason[1]))
			episode, _ := strconv.Atoi(string(searchEpisode[1]))

			if !util.Contains(*seasons, int64(season)) {
				t.App.Log.WithFields(logF).Tracef("unknown season: %d", season)
				// if the season is unknown, add it
				seasonData, err := provider.GetTVSSeason(season)
				if err == nil {
					t.App.DB.AddShowSeason(ctx, database.AddShowSeasonParams{
						Title:       seasonData.Title,
						Overview:    seasonData.Overview,
						Icon:        seasonData.Icon,
						Season:      int64(season),
						Fanart:      seasonData.Fanart,
						Premiered:   seasonData.Premiered,
						Rating:      seasonData.Rating,
						Trailer:     seasonData.Trailer,
						ScraperName: seasonData.ScraperInfo.ScraperName,
						ScraperData: seasonData.ScraperInfo.ScraperData,
						ScraperID:   seasonData.ScraperInfo.ScraperID,
						ScraperLink: seasonData.ScraperInfo.ScraperLink,
						AddDate:     time.Now().Unix(),
						UpdateMode:  -1,
						IDShow:      idshow,
					})
				} else {
					t.App.Log.WithFields(logF).Error(err)
					t.App.DB.AddShowSeason(ctx, database.AddShowSeasonParams{
						Title:       "Season " + strconv.Itoa(season),
						Season:      int64(season),
						ScraperName: seasonData.ScraperInfo.ScraperName,
						ScraperData: seasonData.ScraperInfo.ScraperData,
						ScraperID:   seasonData.ScraperInfo.ScraperID,
						ScraperLink: seasonData.ScraperInfo.ScraperLink,
						AddDate:     time.Now().Unix(),
						UpdateMode:  -1,
						IDShow:      idshow,
					})
				}
				*seasons = append(*seasons, int64(season))
			}

			// add the episode
			epData, err := provider.GetTVSEpisode(season, episode)
			if err == nil {
				t.App.Log.WithFields(logF).Tracef("add episode: %d for season: %d", episode, season)
				idEp, err := t.App.DB.AddShowEpisode(ctx, database.AddShowEpisodeParams{
					Title:       epData.Title,
					Overview:    epData.Overview,
					Icon:        epData.Icon,
					Premiered:   epData.Premiered,
					Rating:      epData.Rating,
					Season:      int64(season),
					Episode:     int64(episode),
					ScraperName: epData.ScraperInfo.ScraperName,
					ScraperID:   epData.ScraperInfo.ScraperID,
					ScraperData: epData.ScraperInfo.ScraperData,
					ScraperLink: epData.ScraperInfo.ScraperLink,
					AddDate:     time.Now().Unix(),
					UpdateMode:  -1,
					IDShow:      idshow,
				})
				if err == nil {
					_, err = file.AddVideoFile(t.App, t.IDLib, p, database.MediaTypeTvsEpisode, idEp, false)
					if err != nil {
						t.App.Log.WithFields(logF).Error(err)
					}
				} else {
					t.App.Log.WithFields(logF).Error(err)
				}
			} else if t.AddUnknown {
				t.App.Log.WithFields(logF).Warn("no data found for s" + strconv.Itoa(season) + "e" + strconv.Itoa(episode) + ", adding empty val")
				// if no data is found but addUnknown is enabled
				idEp, err := t.App.DB.AddShowEpisode(ctx, database.AddShowEpisodeParams{
					Title:      filename,
					AddDate:    time.Now().Unix(),
					UpdateMode: -1,
					Season:     int64(season),
					Episode:    int64(episode),
					IDShow:     idshow,
				})
				if err == nil {
					_, err = file.AddVideoFile(t.App, t.IDLib, p, database.MediaTypeTvsEpisode, idEp, false)
					if err != nil {
						t.App.Log.WithFields(logF).Error(err)
					}
				} else {
					t.App.Log.WithFields(logF).Error(err)
				}
			} else {
				t.App.Log.WithFields(logF).Warn("no data found for s" + strconv.Itoa(season) + "e" + strconv.Itoa(episode))
			}
		} else {
			t.App.Log.WithFields(logF).Warn("unable to extract season/episode info for: " + string(filename))
		}
	}
}

func (t *TVSScraper) UpdateWithSelectionResult(id int64, selection SelectionResult) error {
	ctx := context.Background()
	// update tvs
	err := t.App.DB.UpdateShow(ctx, database.UpdateShowParams{ScraperID: selection.ScraperID, ScraperName: selection.ScraperName, ScraperData: selection.ScraperData, UpdateMode: 1, ID: id})
	if err != nil {
		return err
	}
	// purge outdated data
	// force rescan of seasons and episodes
	err = t.App.DB.UpdateShowAllSeasons(ctx, database.UpdateShowAllSeasonsParams{IDShow: id, ScraperName: " ", ScraperID: "0", UpdateMode: 1})
	if err != nil {
		return err
	}
	err = t.App.DB.UpdateShowAllEpisodes(ctx, database.UpdateShowAllEpisodesParams{IDShow: id, ScraperName: " ", ScraperID: "0", UpdateMode: 1})
	if err != nil {
		return err
	}
	// delete tags and people
	err = t.App.DB.DeleteAllTagLinks(ctx, database.DeleteAllTagLinksParams{MediaType: database.MediaTypeTvs, MediaData: id})
	if err != nil {
		return err
	}
	err = t.App.DB.DeleteAllPersonLinks(ctx, database.DeleteAllPersonLinksParams{MediaType: database.MediaTypeTvs, MediaData: id})
	if err != nil {
		return err
	}
	return nil
}
