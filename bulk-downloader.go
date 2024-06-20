package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
)
var wg sync.WaitGroup

var SERVER_LOCATION string = "https://api.tnris.org"

// Keep track of where the user wants to save the files
var save_dir string = ""

// Flag to stop downloading immediately.
var stop_now bool = false

// Flag to keep track of whether a download is in progress
var running bool = false

// Keep track of how many items are being downloaded simultaneously
var downloading int = 0

// Keep track of how many items have been downloaded
var downloaded int = 0

// Keeps track of all the responses
var currentDownloads []*http.Response

// checkBoxes are different categories you can select to limit what types of files to download.
var checkBoxes []*widget.Check

// sel_ctgry stores the selected category when you check a checkbox.
var sel_ctgry string = ""

// inputWidget keeps track of where we will download the files to.
var inputWidget *widget.Label = widget.NewLabel("Browse to folder to download: \n" + save_dir)

// Embed TXGIO_LOGO.png in binary
//go:embed TXGIO_LOGO.png
var logobytes []byte

// log list of downloads
var logData []string

// Keep track of widget position
var pos float32

var categories *container.Scroll
var outLog *widget.List
var pbar *widget.ProgressBar

type RId struct {
	ResourceTypeName string `json:"resource_type_name"`
	ResourceTypeAbbreviation string `json:"resource_type_abbreviation"`
	ResourceTypeCategory string `json:"resource_type_category"`
}

type RIds struct {
	Ids []RId `json:"results"`
}

type ResourceId struct {
	ResourceId string `json:"resource_id"`
	Resource string `json:"resource"`
}

type DataHubItems struct {
	Results []ResourceId `json:"results"`
	Next string `json:"next"`
}

func configBrowseButton(myWindow fyne.Window) *widget.Button {
	return widget.NewButton("Browse", func() {
		dialog.ShowFolderOpen(func(dir fyne.ListableURI, err error) {
			if err != nil {
				dialog.ShowError(err, myWindow)
				return
			}
			if dir != nil {
				save_dir = dir.Path() // here value of save_dir shall be updated!
				inputWidget.SetText("Browse to folder to download: \n" + save_dir)
			}
		}, myWindow)
	})
}

func configStopButton() *widget.Button {
	return widget.NewButton("Stop", func() {
		cancelDownloads(true)
		stop_now = true
		running = false
	})
}

func configGetDataButton(input *widget.Entry) *widget.Button {
	return widget.NewButton("Get Data", func() {
		// Run download on a seperate thread from the UI
		if(!running) {
			running = true
			go getData(*input)
		}
	})
}

func configLog() *widget.List {

	// List has no onscroll callback api exposed right now. So it must refresh on an interval
	var ticker *time.Ticker = time.NewTicker(500 * time.Millisecond)
	go func() {
		for {
			select {
				case <-ticker.C:
					outLog.Refresh()
			}
		}
	}()

	return widget.NewList(
		func() int {
			return len(logData)
		},
		func() fyne.CanvasObject {
			// Set default message, and color
			return canvas.NewText("LogData", color.White)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			if(strings.HasSuffix(logData[i], " Completed")) {
				// Set text to Green.
				o.(*canvas.Text).Color = color.RGBA{0, 255, 0, 0}
			} else if(strings.HasPrefix(logData[i], "Error: ")) {
				// Set text to Red
				o.(*canvas.Text).Color = color.RGBA{255, 0, 0, 0}
			} else {
				o.(*canvas.Text).Color = color.White
			}
			o.(*canvas.Text).Text = logData[i];
		})
}

// main sets up the GUI and button actions
func main() {
	// Configure the application
	myApp := app.New()
	myApp.Settings().SetTheme(theme.DarkTheme())

	myWindow := myApp.NewWindow("TXGIO DataHub Bulk Download Utility")
	input := widget.NewEntry()
	pbar = widget.NewProgressBar()

	browseButton := configBrowseButton(myWindow)
	stopButton := configStopButton()
	getDataButton := configGetDataButton(input)
	
	var lb = bytes.NewReader(logobytes)
	var logo *canvas.Image = canvas.NewImageFromReader(lb, "TXGIO_LOGO.png")
	logo.FillMode = canvas.ImageFillContain
	contentUUID := container.New(layout.NewGridLayout(3), container.New(layout.NewVBoxLayout(), widget.NewLabel("Enter a TXGIO DataHub Collection ID: ")), container.New(layout.NewVBoxLayout(),input))
	//filterNote := widget.NewLabel("If the collection entered has multiple resource types, filter them here.\nNo filter selection will result in all collection resources downloaded.")
	
	stopStartBtn := container.New(layout.NewGridLayout(2), stopButton, getDataButton)
	smallInLab:= container.New(layout.NewVBoxLayout(), inputWidget)
	smallBrowseButton := container.New(layout.NewVBoxLayout(), browseButton)
	
	inputBrowse := container.New(layout.NewGridLayout(3), smallInLab, smallBrowseButton, layout.NewSpacer())
	
	// Configure the Log
	outLog = configLog()
	
	uuid_input := container.NewVBox(contentUUID, inputBrowse)

	progress_stopStart := container.NewVBox(pbar, stopStartBtn)

	categories = getCategories()

	pos += 200
	progress_stopStart.Resize(fyne.NewSize(1000,100))
	progress_stopStart.Move(fyne.NewPos(0, pos))

	pos += 80
	outLog.Resize(fyne.NewSize(1000,200))
	outLog.Move(fyne.NewPos(0, pos))

	// Position Logo (Not relative to other containers.)
	logo_container := container.New(layout.NewGridLayoutWithColumns(1), logo)
	logo_container.Resize(fyne.NewSize(160,160))
	logo_container.Move(fyne.NewPos(740, -40))


	allstuff := container.NewWithoutLayout(uuid_input, categories, progress_stopStart, outLog, logo_container)
	myWindow.Resize(fyne.NewSize(1000, 600))

	myWindow.SetContent(allstuff)
	myWindow.ShowAndRun()
}

// isValidUUID checks a string and determines whether it's a uuid or not
func IsValidUUID(u string) bool {
    _, err := uuid.Parse(u)
    return err == nil
}

// getResp gets a list of DataHubItems from the url sent in
func getResp(getUrl string, type_query string) *DataHubItems{
	if(sel_ctgry != "") {
		getUrl = getUrl + type_query
	}

	resp, err := http.Get(getUrl)
	if(err != nil) {
		printLog(fmt.Sprintf("Error: %s", err))
	}
	body, err := io.ReadAll(resp.Body)
	if(err != nil) {
		printLog(fmt.Sprintf("Error: %s", err))
	}
	defer resp.Body.Close()

	results := &DataHubItems{}
	json.Unmarshal([]byte(string(body)), results)
	
	return results
}

// getData initiates gathering the list of filebitcoin
		
func getData(input widget.Entry) {
	pbar.SetValue(0.0)
	stop_now = false
	downloaded = 0

	if(!IsValidUUID(input.Text)) {
		running = false
		printLog("Error: TXGIO Datahub Collection ID is invalid.")
		return
	}
	if(len([]rune(save_dir)) == 0) {
		running = false
		printLog("Error: No directory has been chosen.")
		return
	}

	base_url := SERVER_LOCATION + "/api/v1/resources/"
	id_query := "?collection_id="
	type_query := "&resource_type_abbreviation="  + sel_ctgry

	// Build the full url
	getUrl := base_url + id_query + input.Text

	var thing1 *DataHubItems = getResp(getUrl, type_query)
	var allResults []ResourceId
	allResults = thing1.Results

	for (thing1.Next != "") {
		thing1 = getResp(thing1.Next, type_query)
		allResults = append(allResults, thing1.Results...)
	}

	if(len(allResults) <= 0) {
		printLog("Error: No data found.")
	}

	for i := 0; i < len(allResults); i++ {
		// Download 4 items at a time
		if !stop_now && downloading <= 3 {
			downloading++
			wg.Add(1)
			go downloadData(allResults[i].Resource, allResults[i].ResourceId, []int {i+1, len(allResults)})				
		} else if !stop_now {
			downloading++
			wg.Add(1)
			go downloadData(allResults[i].Resource, allResults[i].ResourceId, []int {i+1, len(allResults)})
			wg.Wait()
		} else {
			stop_now = false
			break
		}
	}

	running = false
}

//downloadData downloads each zip file individually
func downloadData(url string, id string, progress []int) {
	resp, err := http.Get(url)

	fnames := strings.Split(url, "/")
	fname := fnames[len(fnames) - 1]
	printLog(fname + " Downloading")

	// Check whether any items in abbr_list are true and add them to resource_type_abbreviations
	currentDownloads = append(currentDownloads, resp)

	if(err != nil) {
		printLog(fmt.Sprintf("Error: %s", err))
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		printLog("Error: Failed to download " + fname + ". Statuscode is: " + fmt.Sprint(resp.StatusCode) + ".")
		exitDownload()
		return
	}

	out, err := os.Create(save_dir + "/" + fname)

	if err != nil {
		printLog(fmt.Sprintf("Error: %s", err))
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	exitDownload()

	//	update download bar.
	var f float64 = float64(downloaded) / float64(progress[1])
	pbar.SetValue(f)
	printLog(fname + " Completed")
	outLog.ScrollToBottom()

	if err != nil {
		printLog(fmt.Sprintf("Error: %s", err))
	}
}

func printLog(msg string) {
	var x string
	// Pop the old messages
	if(len(logData) > 100) {
		x, logData = logData[0], logData[1:]
		_ = x
	}
	logData = append(logData, msg)
	outLog.ScrollToBottom()
}

// exitDownload updates download progress and stops waiting (wg.Done())
func exitDownload() {
	downloaded++
	downloading--
	wg.Done()
}

// cancelDownloads loops over the currentDownloads slice (Each item in the currentDownloads slice is a Http.resp)
// then closes them all and deletes their references from the slice.
// It sets the downloading counter to 0 and clears out the currentDownloads slice
// Use this function to stop all downloads in progress and clean up.
func cancelDownloads(reset bool) {
	for i := 0; i < len(currentDownloads); i++ {
		currentDownloads[i].Body.Close()
	}
	currentDownloads = nil
	if(reset) {
		downloading = 0
	}
}

func getCategories() *container.Scroll {
	base_url := SERVER_LOCATION + "/api/v1/resource_types/"

	resp, err := http.Get(base_url)
	if(err != nil) {
		printLog(fmt.Sprintf("Error: %s", err))
	}
	body, err := io.ReadAll(resp.Body)
	if(err != nil) {
		printLog(fmt.Sprintf("Error: %s", err))
	}

	results := &RIds{}
	json.Unmarshal([]byte(string(body)), results)

	elevationLabel := canvas.NewText("Elevation", color.White)
	elevationLabel.TextStyle = fyne.TextStyle{Bold: true}

	imageryLabel := canvas.NewText("Imagery", color.White)
	imageryLabel.TextStyle = fyne.TextStyle{Bold: true}

	otherLabel := canvas.NewText("Other", color.White)
	otherLabel.TextStyle = fyne.TextStyle{Bold: true}
	elevationBox := container.NewVBox()
	elevationBox.Add(elevationLabel)
	imageryBox := container.NewVBox()

	imageryBox.Add(imageryLabel)
	otherBox := container.NewVBox()
	otherBox.Add(otherLabel)

	for i := 0; i < len(results.Ids); i++ {
		if results.Ids[i].ResourceTypeCategory == "IMAGERY" {
			addCheckToThis(imageryBox, &results.Ids[i])
		} else if results.Ids[i].ResourceTypeCategory == "ELEVATION" {
			addCheckToThis(elevationBox, &results.Ids[i])
		} else {
			addCheckToThis(otherBox, &results.Ids[i])
		}
	}
	scrollbox := container.NewScroll(container.New(layout.NewGridLayout(3), elevationBox, imageryBox, otherBox))

	pos += 100

	scrollbox.Resize(fyne.NewSize(1000,200))
	scrollbox.Move(fyne.NewPos(0, pos))
	return scrollbox
}


// addCheckToThis takes a fyne.Container, and a RId
// It then parsed the RId and adds a checkbox to the fyne.Container
// It also populates the global variable checkBoxes which stores a slice 
// of references to all the checkboxes.
func addCheckToThis(box *fyne.Container, r *RId) {
	var checkbox *widget.Check
	checkbox = widget.NewCheck(r.ResourceTypeName, func(value bool) {
		if(value) { // If checked (Not unchecked)
			unselect_all_except(checkbox)
			sel_ctgry = r.ResourceTypeAbbreviation
		} else {
			sel_ctgry = ""
		}
	})
	box.Add(checkbox)
	checkBoxes = append(checkBoxes, checkbox) // Store a slice of all the checkboxes to easily uncheck all.
}

// unselect_all_except Unselects all of the checkboxes. Except for the one passed in. 
func unselect_all_except(this_check *widget.Check) {
	for i := 0; i < len(checkBoxes); i++ {
		if(this_check == checkBoxes[i]) {
			continue
		} else {
			checkBoxes[i].SetChecked(false)
		}
	}
}
