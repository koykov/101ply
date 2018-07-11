// Console player for 101.ru online radio station.
// Made just for fun and personal comfort.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"flag"

	"github.com/PuerkitoBio/goquery"
	"github.com/fsnotify/fsnotify"

	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/keybind"
	"github.com/BurntSushi/xgbutil/xevent"

	mp3 "github.com/koykov/mp3lib"
)

const (
	STATUS_PLAY   = 0x100
	STATUS_PAUSE  = 0x200
	STATUS_STOP   = 0x300
)

// Error handling types
type Block struct {
	Try     func()
	Catch   func(Exception)
	Finally func()
}

type Exception interface{}

// JSON types
type Hotkey struct {
	Key					string `json:"key"`
	Desc				string `json:"desc"`
}

type TrackInfo struct {
	Status				uint64 `json:"status"`
	Result				TrackInfo__Result `json:"result"`
	ErrorCode			uint64 `json:"errorCode"`
}

type TrackInfo__Result struct {
	About				TrackInfo__Result__About `json:"about"`
	Stat				TrackInfo__Result__Stat `json:"stat"`
}

type TrackInfo__Result__About struct {
	Title 				string `json:"title"`
	Artist				string `json:"title_executor"`
	Audio				[]TrackInfo__Result__About__Audio `json:"audio"`
	Album				TrackInfo__Result__About__Album `json:"album"`
}

type TrackInfo__Result__About__Audio struct {
	TrackUid			uint64 `json:"trackuid"`
	Filename			string `json:"filename"`
}

type TrackInfo__Result__About__Album struct {
	Title				string `json:"title"`
	ReleaseDate			string `json:"releaseDate"`
}

type TrackInfo__Result__Stat struct {
	StartSong			uint64 `json:"startSong"`
	FinishSong			uint64 `json:"finishSong"`
	ServerTime			uint64 `json:"serverTime"`
}

// General types
type go101 struct {
	ChannelGroups		map[uint64]go101ChannelGroup
	ChannelGroupsUrl	string
	CurrentGroup		uint64
	CurrentChannel		uint64
	CurrentTrack		go101TrackInfo
	TrackUid			uint64
	Status				uint64
	NextFetch			uint64
}

type go101TrackInfo struct {
	TrackUid			uint64
	Artist				string
	Title				string
	Album				string
	AlbumDate			string
	PlayURL				string
}

type go101Channel struct {
	Id					uint64 `json:"Id"`
	Title				string `json:"Title"`
}

type go101ChannelGroup struct {
	Id					uint64 `json:"Id"`
	Title				string `json:"Title"`
	Channels			map[uint64]go101Channel `json:"Channels"`
}

var go101o go101
var verbose bool

func init() {
	// Check (and create if needed) configuration directory.
	configDir := GetConfigDir()
	_, err := os.Stat(configDir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(configDir, 0755); err != nil {
			log.Fatal("Cannot create configuration diectory.")
		}
	}
	// Check (and create) hotkeys configuration file.
	hotkeyConfig := GetHotkeyConfig()
	_, err = os.Stat(hotkeyConfig)
	if os.IsNotExist(err) {
		// For possible keys see https://github.com/BurntSushi/xgbutil/blob/master/keybind/keysymdef.go
		// Unfortunately, there isn't possibility to specify a key combination, only one key may be used.
		PutToFile(hotkeyConfig, `[
	{
		"key": "Pause",
		"desc": "Play/pause."
	}
]`)
		Debug("create default config file - %s", hotkeyConfig)
	}
	// Check (and create if needed) cache directory.
	cacheDir := GetCacheDir()
	_, err = os.Stat(cacheDir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Fatal("Cannot create cache diectory.")
		}
	}
}

func main() {
	var wg sync.WaitGroup

	// Parse CLI options.
	channelPtr := flag.Int("c", 0, "Channel ID.")
	verbosePtr := flag.Bool("verbose", false, "Display debug messages.")
	flag.Parse()

	verbose = *verbosePtr

	// Make goroutine for final cleanup callback.
	wg.Add(1)
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer wg.Done()
		<-c
		Cleanup()
		os.Exit(1)
	}()

	// Initialize keybinding.
	X, err := xgbutil.NewConn()
	if err != nil {
		log.Fatal(err)
	}
	keybind.Initialize(X)

	hotkeyConfig := GetHotkeyConfig()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	err = watcher.Add(hotkeyConfig)
	if err != nil {
		log.Println(err)
	}

	// Keybinding goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case ev := <-watcher.Events:
				log.Println(ev)
				err := bindall(hotkeyConfig, X)
				if err != nil {
					log.Println(err)
					continue
				}

			case err := <-watcher.Errors:
				log.Println("error:", err)
			}
		}
	}()
	err = bindall(hotkeyConfig, X)
	if err != nil {
		log.Panicln(err)
	}

	// Event handling goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		xevent.Main(X)
	}()

	// Cache check.
	cacheFile := GetCacheDir() + string(os.PathSeparator) + "data.json"
	needRegenerate := false
	fi, err := os.Stat(cacheFile)
	if err != nil {
		if os.IsNotExist(err) {
			needRegenerate = true
			Debug("Cache file %s doesn't exists, need generate.", cacheFile)
		} else {
			log.Fatal("Error when reading cache file: %s", err.Error())
		}
	}
	if !needRegenerate {
		now := time.Now()
		mtime := fi.ModTime()
		diff := now.Sub(mtime)
		needRegenerate = diff.Seconds() > 7*24*3600
		if needRegenerate {
			Debug("Cache file %s is deprecated, need regenerate.", cacheFile)
		}
	}
	if !needRegenerate {
		// Read channels and groups from the cache.
		raw, err := ioutil.ReadFile(cacheFile)
		if err != nil {
			log.Fatal("Error reading cache file: %s", err.Error())
		}
		go101o.ChannelGroups = make(map[uint64]go101ChannelGroup)
		json.Unmarshal(raw, &go101o.ChannelGroups)
		Debug("Cache hit, reading file %s", cacheFile)
	} else {
		// Fetch channels and groups from 101.ru
		go101o.FetchChannelGroups()
		go101o.FetchChannels()

		b, err := json.Marshal(go101o.ChannelGroups)
		if err != nil {
			log.Fatal(err.Error())
		}

		PutToFile(cacheFile, string(b))
		Debug("Write groups and channels data to cache file %s", cacheFile)
	}
	//fmt.Printf("%#v\n", go101o)

	// Choose group and channel.
	if *channelPtr == 0 {
		reader := bufio.NewReader(os.Stdin)

		gls := make([]string, len(go101o.ChannelGroups))
		for _, g := range go101o.ChannelGroups {
			gls = append(gls, fmt.Sprintf("%d - %s\n", g.Id, g.Title))
		}
		sort.Strings(gls)
		fmt.Println("Choose group:")
		for _, g := range gls {
			fmt.Print(g)
		}
		fmt.Print("\nGroup: ")
		groupIndex, _ := reader.ReadString('\n')
		go101o.CurrentGroup, _ = strconv.ParseUint(strings.Trim(groupIndex, "\n"), 10, 64)

		cls := make([]string, len(go101o.ChannelGroups[go101o.CurrentGroup].Channels))
		for _, c := range go101o.ChannelGroups[go101o.CurrentGroup].Channels {
			cls = append(cls, fmt.Sprintf("%d - %s\n", c.Id, c.Title))
		}
		sort.Strings(cls)
		fmt.Println("\nChoose channel:")
		for _, c := range cls {
			fmt.Print(c)
		}
		fmt.Print("\nChannel: ")
		channelIndex, _ := reader.ReadString('\n')
		go101o.CurrentChannel, _ = strconv.ParseUint(strings.Trim(channelIndex, "\n"), 10, 64)
	} else {
		for gid, _ := range go101o.ChannelGroups {
			for cid, _ := range go101o.ChannelGroups[gid].Channels {
				if cid == uint64(*channelPtr) {
					go101o.CurrentGroup = gid
				}
			}
		}
		go101o.CurrentChannel = uint64(*channelPtr)
	}
	group := go101o.ChannelGroups[go101o.CurrentGroup]
	channel := group.Channels[go101o.CurrentChannel]

	// Playing loop.
	fmt.Printf("\nPlayng: %s\n", channel.Title)
	for true {
		go101o.FetchChannelInfo()
		if go101o.TrackUid != go101o.CurrentTrack.TrackUid {
			fmt.Printf("%s - %s [%s] - %s\n", go101o.CurrentTrack.Artist, go101o.CurrentTrack.Title, go101o.CurrentTrack.Album, FormatTime(go101o.NextFetch))
			Debug("Fetch remote data %#v", go101o.CurrentTrack)
			go101o.Stop()
			go go101o.Play()
		}
		Debug("Next fetch after %d seconds", go101o.NextFetch)
		go101o.Sleep(go101o.NextFetch)
	}

	// Waiting for finishing all goroutines.
	wg.Wait()
}

// Process finish callback.
func Cleanup() {
	go101o.Stop()
	Debug("Cleanup sig.")
}

// Returns full path to the config directory.
func GetConfigDir() string {
	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	ps := string(os.PathSeparator)
	return usr.HomeDir + ps + ".config" + ps + "101ply"
}

// Returns full path to the hotkey configuration file.
func GetHotkeyConfig() string {
	ps := string(os.PathSeparator)
	return GetConfigDir() + ps + "hotkey.json"
}

// Returns full path to the cache directory.
func GetCacheDir() string {
	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	ps := string(os.PathSeparator)
	return usr.HomeDir + ps + ".cache" + ps + "101ply"
}

// Create file (if needed) and write contents to him.
func PutToFile(filename string, contents string) {
	if _, err := os.Create(filename); err != nil {
		log.Fatal("Error when file is created: ", err.Error())
	}

	file, err := os.OpenFile(filename, os.O_RDWR, 0644)
	if err != nil {
		log.Fatal("Error when file is created: ", err.Error())
	}
	defer file.Close()
	file.WriteString(contents)
	if err = file.Sync(); err != nil {
		log.Fatal("Error when saving file: ", err.Error())
	}
}

// Parses config file and binds keys to events.
func bindall(hotkeyConfig string, X *xgbutil.XUtil) (err error) {
	config, err := ioutil.ReadFile(hotkeyConfig)
	if err != nil {
		log.Fatal("Could not find config file: ", err.Error())
		return
	}
	hotkeys := []Hotkey{}
	err = json.Unmarshal(config, &hotkeys)
	if err != nil {
		log.Fatal("Could not parse config file: ", err.Error())
		return
	}
	keybind.Detach(X, X.RootWin())
	for _, hotkey := range hotkeys {
		hotkey.attach(X)
	}
	return
}

// Attach callback to the hotkey.
func (hotkey Hotkey) attach(X *xgbutil.XUtil) {
	err := keybind.KeyPressFun(
		func(X *xgbutil.XUtil, e xevent.KeyPressEvent) {
			if (go101o.Status == STATUS_STOP || go101o.Status == STATUS_PAUSE) {
				go go101o.Resume()
			} else {
				go go101o.Pause()
			}
		}).Connect(X, X.RootWin(), hotkey.Key, true)
	if err != nil {
		log.Fatalf("Could not bind %s: %s", hotkey.Key, err.Error())
	}
}

// Convert seconds to "mm:ss" time format.
func FormatTime(s uint64) (string) {
	min := s / 60
	sec := s % 60
	return fmt.Sprintf("%d:%d", min, sec)
}

// Print formatted debug message.
func Debug(message string, a ...interface{}) {
	if verbose {
		fmt.Println(fmt.Sprintf("Debug: " + message, a))
	}
}

// Fetches channel groups from 101.ru
func (this *go101) FetchChannelGroups() {
	this.ChannelGroups = make(map[uint64]go101ChannelGroup)

	doc, err := goquery.NewDocument("http://101.ru/radio-top")
	if err != nil {
		log.Fatal("Couldn't fetch channel groups: ", err.Error())
	}
	doc.Find("ul.full.list.menu li").Each(func(i int, selection *goquery.Selection) {
		title := selection.Find("a").Text()
		href, exists := selection.Find("a").Attr("href")
		if exists {
			id, _ := strconv.ParseUint(path.Base(href), 0, 64)
			channels := make(map[uint64]go101Channel, 0)
			this.ChannelGroups[id] = go101ChannelGroup{
				id, title, channels,
			}
		}
	})
}

// Fetches channels from 101.ru
func (this *go101) FetchChannels() {
	for gid, cg := range this.ChannelGroups {

		doc, err := goquery.NewDocument(fmt.Sprintf("http://101.ru/radio-group/group/%d", cg.Id))
		if err != nil {
			log.Fatal("Couldn't fetch channels: ", err.Error())
		}

		doc.Find("ul.list.list-channels li").Each(func(i int, selection *goquery.Selection) {
			title := selection.Find("a").Find(".h3").Text()
			href, exists := selection.Find("a").Attr("href")
			if exists {
				cid, _ := strconv.ParseUint(path.Base(href), 0, 64)
				this.ChannelGroups[gid].Channels[cid] = go101Channel{
					cid, title,
				}
			}
		})
	}
}

// Fetch channel info.
func (this *go101) FetchChannelInfo() {
	Block{
		Try: func() {
			playlistUrl := fmt.Sprintf("http://101.ru/api/channel/getTrackOnAir/%d/channel/?dataFormat=json", this.CurrentChannel)
			response, err := http.Get(playlistUrl)
			if err != nil {
				panic(err)
				//return
			}
			defer response.Body.Close()

			b, err := ioutil.ReadAll(response.Body)
			if err != nil {
				panic(err)
			}

			var trackInfo TrackInfo
			err = json.Unmarshal(b, &trackInfo)
			if err != nil {
				panic(err)
			}
			this.CurrentTrack.TrackUid = trackInfo.Result.About.Audio[0].TrackUid
			this.CurrentTrack.Title= trackInfo.Result.About.Title
			this.CurrentTrack.Artist = trackInfo.Result.About.Artist
			this.CurrentTrack.Album = trackInfo.Result.About.Album.Title
			this.CurrentTrack.AlbumDate = trackInfo.Result.About.Album.ReleaseDate

			// Provide case when got full URL.
			re := regexp.MustCompile(`http\:(.)`)
			res := re.FindStringSubmatch(string(trackInfo.Result.About.Audio[0].Filename))
			prefix := "";
			if res == nil {
				prefix = "http://101.ru"
			}
			this.CurrentTrack.PlayURL = prefix + trackInfo.Result.About.Audio[0].Filename

			// Provide case with wrong URL (ex: http://cdn*.101.ru/vardata/modules/musicdb/files//vardata/modules/musicdb/files/*).
			//                                                    ^                             ^^
			re = regexp.MustCompile(`(\/vardata\/modules\/musicdb\/files\/)`)
			dres := re.FindAllStringSubmatch(string(this.CurrentTrack.PlayURL), -1)
			if (len(dres) == 2) {
				this.CurrentTrack.PlayURL = strings.Replace(this.CurrentTrack.PlayURL, "/vardata/modules/musicdb/files/", "", 1)
			}

			// Calculate next fetch period. Based on the difference between current timestamp and song start timestamp.
			diff := uint64(trackInfo.Result.Stat.FinishSong - trackInfo.Result.Stat.ServerTime)
			if diff < 5 || diff > 1800 {
				diff = 5
			} else {
				diff -= 3
			}
			this.NextFetch = diff
		},
		Catch: func(e Exception) {
			Debug("Got error during fetch channel info: %s", e)
			this.NextFetch = 5
		},
		Finally: func() {
			// Normal behavior...
		},
	}.Do()
}

// Play channel.
func (this *go101) Play() {
	playUrl := this.CurrentTrack.PlayURL
	mp3.PlayProcess(playUrl)
	this.TrackUid = this.CurrentTrack.TrackUid
	if this.Status == STATUS_PAUSE {
		this.Pause()
	} else {
		this.Status = STATUS_PLAY
		Debug("Play sig.")
	}
}

// Pause playing.
func (this *go101) Pause() {
	// Since we plays music from online radio station, it make sense to just mute sound.
	// At the resume signal we will continue from actual moment of station playing.
	mp3.MuteProcess()
	this.Status = STATUS_PAUSE
	Debug("Pause sig.")
}

// Resume playing.
func (this *go101) Resume() {
	// See go101ply.Pause()
	mp3.UnmuteProcess()
	this.Status = STATUS_PLAY
	Debug("Resume sig.")
}

// Stop playing.
func (this *go101) Stop() {
	// Call stop proc twice, just in case.
	mp3.StopProcess()
	mp3.StopProcess()
	this.Status = STATUS_STOP
	Debug("Stop sig.")
}

// Sleep function, freezes duration on pause/stop status.
func (this *go101) Sleep(s uint64) {
	var counter uint64
	for true {
		time.Sleep(time.Second)
		if this.Status == STATUS_PLAY {
			counter += 1
		}
		if counter >= s {
			break
		}
	}
}

func (this Block) Do() {
	if this.Finally != nil {
		defer this.Finally()
	}
	if this.Catch != nil {
		defer func() {
			if r := recover(); r != nil {
				this.Catch(r)
			}
		}()
	}
	this.Try()
}