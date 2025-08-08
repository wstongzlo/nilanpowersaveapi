package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gocolly/colly"
	"github.com/pjuzeliunas/nilan"
	"github.com/spf13/viper"
	"gopkg.in/natefinch/lumberjack.v2"
)

type Readings struct {
	RoomTemperature          int `json:"RoomTemperature"`
	OutdoorTemperature       int `json:"OutdoorTemperature"`
	AverageHumidity          int `json:"AverageHumidity"`
	ActualHumidity           int `json:"ActualHumidity"`
	DHWTankTopTemperature    int `json:"DHWTankTopTemperature"`
	DHWTankBottomTemperature int `json:"DHWTankBottomTemperature"`
	SupplyFlowTemperature    int `json:"SupplyFlowTemperature"`
}

type Settings struct {
	FanSpeed                    int  `json:"FanSpeed"`
	DesiredRoomTemperature      int  `json:"DesiredRoomTemperature"`
	DesiredDHWTemperature       int  `json:"DesiredDHWTemperature"`
	DHWProductionPaused         bool `json:"DHWProductionPaused"`
	DHWProductionPauseDuration  int  `json:"DHWProductionPauseDuration"`
	CentralHeatingPaused        bool `json:"CentralHeatingPaused"`
	CentralHeatingPauseDuration int  `json:"CentralHeatingPauseDuration"`
	CentralHeatingIsOn          bool `json:"CentralHeatingIsOn"`
	VentilationMode             int  `json:"VentilationMode"`
	VentilationOnPause          bool `json:"VentilationOnPause"`
	SetpointSupplyTemperature   int  `json:"SetpointSupplyTemperature"`
}

type Config struct {
	IsAutoSavePowerMode           bool `json:"isAutoSavePowerMode"`
	RunHours                      int  `json:"runHours"`
	MustHeatTemperatureDifference int  `json:"mustHeatTemperatureDifference"`
	StopHeatTemperatureDifference int  `json:"stopHeatTemperatureDifference"`
}

type PriceData struct {
	HourlyPrices      []float64 `json:"hourlyPrices"`
	LowestPriceHours  []int     `json:"lowestPriceHours"`
	LowestPriceValues []float64 `json:"lowestPriceValues"`
}

var (
	mu                            sync.Mutex
	isAutoSavePowerMode           bool
	runHours                      int
	mustHeatTemperatureDifference int
	stopHeatTemperatureDifference int
	apiBaseURL                    = "http://localhost:8082"
	lastPriceData                 PriceData
)

func initLogger() {
	if _, err := os.Stat("log"); os.IsNotExist(err) {
		err := os.Mkdir("log", os.ModePerm)
		if err != nil {
			log.Fatalf("failed to create log directory: %v", err)
		}
	}
	ex, _ := os.Executable()
	logDir := filepath.Join(filepath.Dir(ex), "log")
	log.SetOutput(&lumberjack.Logger{
		Filename:   logDir + "/nilanlogfile" + time.Now().Format("2006-01-02") + ".log",
		MaxSize:    10, // MB
		MaxBackups: 3,
		MaxAge:     28, // days
		Compress:   true,
	})
	log.SetFlags(log.LstdFlags)
	fmt.Print(logDir)
}

func loadConfig() error {
	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	if err := viper.ReadInConfig(); err != nil {
		return fmt.Errorf("failed to read config file: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	isAutoSavePowerMode = viper.GetBool("savemode.on")
	runHours = viper.GetInt("setting.runhours")
	mustHeatTemperatureDifference = viper.GetInt("setting.mustheatdf")
	stopHeatTemperatureDifference = viper.GetInt("setting.stopheatdf")
	return nil
}

func saveConfig() error {
	mu.Lock()
	defer mu.Unlock()
	viper.Set("savemode.on", isAutoSavePowerMode)
	viper.Set("setting.runhours", runHours)
	viper.Set("setting.mustheatdf", mustHeatTemperatureDifference)
	viper.Set("setting.stopheatdf", stopHeatTemperatureDifference)
	return viper.WriteConfig()
}

func nilanController() nilan.Controller {
	conf := nilan.CurrentConfig()
	return nilan.Controller{Config: conf}
}

var runOnce, initialOnce bool = true, true

func autoConfigure(freq time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("autoConfigure panicked: %v", r)
			time.Sleep(freq)
			autoConfigure(freq)
		}
	}()

	c := nilanController()

	lowestPrices := make([]float64, runHours)
	lowestHours := make([]int, runHours)

	for {
		dt := time.Now()
		log.Printf("Checking for price update at hour %d, runOnce: %t", dt.Local().Hour(), runOnce)

		if (dt.Local().Hour() == 20 && runOnce) || initialOnce {
			scrapURL := "https://andelenergi.dk/kundeservice/aftaler-og-priser/timepris/"
			var err error
			lowestHours, lowestPrices, err = GetLowestPriceHours(scrapURL, runHours)
			if err != nil {
				log.Printf("Failed to get lowest price hours: %v", err)
				time.Sleep(freq)
				continue
			}
			log.Printf("Lowest price hours: %v, prices: %v", lowestHours, lowestPrices)
			runOnce = false
		} else if dt.Local().Hour() != 20 {
			runOnce = true
		}
		initialOnce = false

		r, err := c.FetchReadings()
		if err != nil {
			log.Printf("Failed to fetch readings: %v", err)
			time.Sleep(freq)
			continue
		}
		s, err := c.FetchSettings()
		if err != nil {
			log.Printf("Failed to fetch settings: %v", err)
			time.Sleep(freq)
			continue
		}

		inHoursHeating := false
		for i := 0; i < runHours; i++ {
			if lowestHours[i] == dt.Local().Hour() {
				inHoursHeating = true
				break
			}
		}

		mu.Lock()
		updateNeeded := false
		if inHoursHeating || (*s.DesiredDHWTemperature-r.DHWTankTopTemperature)/10 >= mustHeatTemperatureDifference {
			log.Printf("Enabling hot water: DesiredDHWTemperature=%d, Actual=%d, Paused=%v",
				*s.DesiredDHWTemperature, r.DHWTankTopTemperature, *s.DHWProductionPaused)
			if *s.DHWProductionPaused && isAutoSavePowerMode {
				*s.DHWProductionPaused = false
				*s.DHWProductionPauseDuration = 0
				updateNeeded = true
			}
		} else if (*s.DesiredDHWTemperature-r.DHWTankTopTemperature)/10 < stopHeatTemperatureDifference && !*s.DHWProductionPaused && isAutoSavePowerMode {
			log.Printf("Pausing hot water: DesiredDHWTemperature=%d, Actual=%d, Paused=%v",
				*s.DesiredDHWTemperature, r.DHWTankTopTemperature, *s.DHWProductionPaused)
			*s.DHWProductionPaused = true
			*s.DHWProductionPauseDuration = 180
			updateNeeded = true
		}
		if updateNeeded {

			log.Printf("Hot water production %s", map[bool]string{true: "paused", false: "enabled"}[*s.DHWProductionPaused])
			c.SendSettings(nilan.Settings{
				DHWProductionPaused:        s.DHWProductionPaused,
				DHWProductionPauseDuration: s.DHWProductionPauseDuration,
			})
		}

		mu.Unlock()

		time.Sleep(freq)
	}
}

func GetLowestPriceHours(scrapURL string, runHours int) ([]int, []float64, error) {
	if runHours <= 0 || runHours > 24 {
		return nil, nil, fmt.Errorf("invalid runHours: %d, must be between 1 and 24", runHours)
	}

	type DateAndDay struct {
		Date string `json:"date"`
		Day  string `json:"day"`
	}
	type Earea struct {
		Labels             []string     `json:"labels"`
		Values             []string     `json:"values"`
		ValuesDistribution []string     `json:"valuesDistribution"`
		Dates              []DateAndDay `json:"dates"`
	}
	type Eall struct {
		East Earea `json:"east"`
		West Earea `json:"west"`
	}

	c := colly.NewCollector()
	minvalue := make([]float64, runHours)
	minhour := make([]int, runHours)
	hourlyPrices := make([]float64, 24)
	for i := 0; i < runHours; i++ {
		minvalue[i] = 9999
		minhour[i] = -1
	}
	for i := 0; i < 24; i++ {
		hourlyPrices[i] = -1
	}

	c.OnHTML("div#chart-component", func(e *colly.HTMLElement) {
		priceJson := e.Attr("data-chart")
		var str Eall
		if err := json.Unmarshal([]byte(priceJson), &str); err != nil {
			log.Printf("Failed to parse price JSON: %v", err)
			return
		}

		if len(str.East.Dates) == 0 || len(str.East.Values) < 24 {
			log.Println("Invalid price data: insufficient dates or values")
			return
		}
		if strings.TrimSpace(str.East.Dates[len(str.East.Dates)-1].Day) == strconv.Itoa(time.Now().Day()) { // Only have current day's data
			for i := 0; i < runHours; i++ {
				for j := 0; j < 24; j++ {
					s1, _ := strconv.ParseFloat(str.East.Values[len(str.East.Values)-28+j], 64)
					ete, _ := strconv.ParseFloat(str.East.ValuesDistribution[len(str.East.ValuesDistribution)-28+j], 64) // add transport expense
					s1 = s1 + ete
					hourlyPrices[j] = s1
					hasCompared := false
					for k := 0; k <= i; k++ {
						if j >= 0 && j < 4 {
							if j+20 == minhour[k] {
								hasCompared = true
							}
						} else {
							if j-4 == minhour[k] {
								hasCompared = true
							}
						}

					}
					if !hasCompared {

						if s1 < minvalue[i] {
							if j >= 0 && j < 4 {
								minvalue[i] = s1
								minhour[i] = j + 20
							} else {
								minvalue[i] = s1
								minhour[i] = j - 4
							}
						}
					}

				}

			}
		} else if strings.TrimSpace(str.East.Dates[len(str.East.Dates)-1].Day) == strconv.Itoa(time.Now().Day()+1) { // Tomorrow's data is available
			if time.Now().Local().Hour() < 20 { // current time is before 20:00
				for i := 0; i < runHours; i++ {
					for j := 0; j < 24; j++ {
						s1, _ := strconv.ParseFloat(str.East.Values[len(str.East.Values)-52+j], 64)
						ete, _ := strconv.ParseFloat(str.East.ValuesDistribution[len(str.East.ValuesDistribution)-52+j], 64) // add transport expense
						s1 = s1 + ete
						hourlyPrices[j] = s1
						hasCompared := false
						for k := 0; k <= i; k++ {
							if j >= 0 && j < 4 {
								if j+20 == minhour[k] {
									hasCompared = true
								}
							} else {
								if j-4 == minhour[k] {
									hasCompared = true
								}
							}
						}
						if !hasCompared {

							if s1 < minvalue[i] {
								if j >= 0 && j < 4 {
									minvalue[i] = s1
									minhour[i] = j + 20
								} else {
									minvalue[i] = s1
									minhour[i] = j - 4
								}

							}
						}

					}

				}
			} else { // current time is after the 20:00
				for i := 0; i < runHours; i++ {
					for j := 0; j < 24; j++ {
						hasCompared := false
						s1, _ := strconv.ParseFloat(str.East.Values[len(str.East.Values)-28+j], 64)
						ete, _ := strconv.ParseFloat(str.East.ValuesDistribution[len(str.East.ValuesDistribution)-28+j], 64) // add transport expense
						s1 = s1 + ete
						hourlyPrices[j] = s1

						if j <= 23 && j >= 4 {
							for k := 0; k <= i; k++ {
								if j-4 == minhour[k] {
									hasCompared = true
								}
							}
							if !hasCompared {

								if s1 < minvalue[i] {
									minvalue[i] = s1
									minhour[i] = j - 4
								}
							}
						} else {
							for k := 0; k <= i; k++ {
								if j+20 == minhour[k] {
									hasCompared = true
								}
							}
							if !hasCompared {
								if s1 < minvalue[i] {
									minvalue[i] = s1
									minhour[i] = j + 20
								}
							}
						}
					}
				}
			}

		}

		mu.Lock()
		lastPriceData = PriceData{
			HourlyPrices:      hourlyPrices,
			LowestPriceHours:  minhour,
			LowestPriceValues: minvalue,
		}
		mu.Unlock()
	})

	c.OnRequest(func(r *colly.Request) {
		log.Printf("Visiting %s", r.URL)
	})
	c.OnError(func(r *colly.Response, err error) {
		log.Printf("Scraping error: %v", err)
	})

	if err := c.Visit(scrapURL); err != nil {
		return nil, nil, fmt.Errorf("failed to visit %s: %v", scrapURL, err)
	}

	if minhour[0] == -1 {
		return nil, nil, fmt.Errorf("no valid price data found")
	}

	return minhour, minvalue, nil
}

func getPrices(c *gin.Context) {
	log.Printf("Processing prices GET request from %v", c.Request.RemoteAddr)
	mu.Lock()
	data := lastPriceData
	mu.Unlock()
	if len(data.HourlyPrices) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "No price data available"})
		return
	}
	c.JSON(http.StatusOK, data)
}

// CORS Middleware
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func readings(c *gin.Context) {
	log.Printf("Processing readings GET request from %v", c.Request.RemoteAddr)
	controller := nilanController()
	readings, err := controller.FetchReadings()
	if err != nil {
		log.Printf("Failed to fetch readings: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch readings"})
		return
	}
	c.JSON(http.StatusOK, readings)
}

func settings(c *gin.Context) {
	log.Printf("Processing settings GET request from %v", c.Request.RemoteAddr)
	controller := nilanController()
	settings, err := controller.FetchSettings()
	if err != nil {
		log.Printf("Failed to fetch settings: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch settings"})
		return
	}
	c.JSON(http.StatusOK, settings)
}

func updateSettings(c *gin.Context) {
	log.Printf("Processing settings update request from %v", c.Request.RemoteAddr)
	controller := nilanController()
	var newSettings nilan.Settings

	if err := c.ShouldBindJSON(&newSettings); err != nil {
		log.Printf("Bad request: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON format"})
		return
	}

	if err := controller.SendSettings(newSettings); err != nil {
		log.Printf("Failed to send settings: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Settings updated successfully"})
}

func getConfig(c *gin.Context) {
	log.Printf("Processing config GET request from %v", c.Request.RemoteAddr)
	mu.Lock()
	config := Config{
		IsAutoSavePowerMode:           isAutoSavePowerMode,
		RunHours:                      runHours,
		MustHeatTemperatureDifference: mustHeatTemperatureDifference,
		StopHeatTemperatureDifference: stopHeatTemperatureDifference,
	}
	mu.Unlock()
	c.JSON(http.StatusOK, config)
}

func updateConfig(c *gin.Context) {
	log.Printf("Processing config update request from %v", c.Request.RemoteAddr)
	var newConfig Config
	if err := c.ShouldBindJSON(&newConfig); err != nil {
		log.Printf("Bad request: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON format"})
		return
	}

	if newConfig.RunHours < 1 || newConfig.RunHours > 24 {
		log.Printf("Invalid runHours: %d", newConfig.RunHours)
		c.JSON(http.StatusBadRequest, gin.H{"error": "runHours must be between 1 and 24"})
		return
	}
	if newConfig.MustHeatTemperatureDifference < 0 {
		log.Printf("Invalid mustHeatTemperatureDifference: %d", newConfig.MustHeatTemperatureDifference)
		c.JSON(http.StatusBadRequest, gin.H{"error": "mustHeatTemperatureDifference must be non-negative"})
		return
	}
	if newConfig.StopHeatTemperatureDifference < 0 {
		log.Printf("Invalid stopHeatTemperatureDifference: %d", newConfig.StopHeatTemperatureDifference)
		c.JSON(http.StatusBadRequest, gin.H{"error": "stopHeatTemperatureDifference must be non-negative"})
		return
	}

	mu.Lock()
	isAutoSavePowerMode = newConfig.IsAutoSavePowerMode
	runHours = newConfig.RunHours
	mustHeatTemperatureDifference = newConfig.MustHeatTemperatureDifference
	stopHeatTemperatureDifference = newConfig.StopHeatTemperatureDifference
	mu.Unlock()

	if err := saveConfig(); err != nil {
		log.Printf("Failed to save config: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save config"})
		return
	}

	if err := loadConfig(); err != nil {
		log.Printf("Failed to reload config: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reload config"})
		return
	}
	initialOnce = true
	c.JSON(http.StatusOK, gin.H{"message": "Config updated successfully"})
}

func main() {
	initLogger()
	log.Println("Starting Nilan control program")

	if err := loadConfig(); err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	startupDelay := viper.GetDuration("setting.startup_delay")
	log.Printf("Waiting for startup delay: %v", startupDelay)
	time.Sleep(startupDelay)

	go autoConfigure(60 * time.Second)

	conf := nilan.CurrentConfig()
	log.Printf("Nilan address: %v", conf.NilanAddress)

	router := gin.Default()
	router.Use(CORSMiddleware())
	router.GET("/readings", readings)
	router.GET("/settings", settings)
	router.PUT("/settings", updateSettings)
	router.GET("/config", getConfig)
	router.PUT("/config", updateConfig)
	router.GET("/prices", getPrices)
	gin.SetMode(gin.ReleaseMode)
	log.Println("Listening at :8082...")
	if err := router.Run(":8082"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
