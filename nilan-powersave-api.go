package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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

var (
	mu                            sync.Mutex
	isAutoSavePowerMode           bool
	runHours                      int
	mustHeatTemperatureDifference int
	stopHeatTemperatureDifference int
	apiBaseURL                    = "http://localhost:8082"
)

func initLogger() {
	log.SetOutput(&lumberjack.Logger{
		Filename:   "/home/kevin/nilan-log/nilanlogfile" + time.Now().Format("2006-01-02") + ".log",
		MaxSize:    10, // MB
		MaxBackups: 3,
		MaxAge:     28, // days
		Compress:   true,
	})
	log.SetFlags(log.LstdFlags)
}

func loadConfig() error {
	viper.SetConfigName("config")
	viper.AddConfigPath("/home/kevin/nilan-hk")
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

func fetchSettings() (Settings, error) {
	var settings Settings
	resp, err := http.Get(apiBaseURL + "/settings")
	if err != nil {
		return settings, fmt.Errorf("failed to fetch settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return settings, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&settings); err != nil {
		return settings, fmt.Errorf("failed to parse settings JSON: %v", err)
	}
	return settings, nil
}

func updateHotWaterSettings(settings Settings) error {
	update := map[string]interface{}{
		"DHWProductionPaused":        settings.DHWProductionPaused,
		"DHWProductionPauseDuration": settings.DHWProductionPauseDuration,
	}
	data, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %v", err)
	}
	req, err := http.NewRequest("PUT", apiBaseURL+"/settings", bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

func nilanController() nilan.Controller {
	conf := nilan.CurrentConfig()
	return nilan.Controller{Config: conf}
}

func autoConfigure(freq time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("autoConfigure panicked: %v", r)
			time.Sleep(freq)
			autoConfigure(freq)
		}
	}()

	c := nilanController()
	var runOnce, initialOnce bool = true, true
	lowestThreePrices := make([]float64, runHours)
	lowestThreeHours := make([]int, runHours)

	for {
		dt := time.Now()
		log.Printf("Checking for price update at hour %d, runOnce: %t", dt.Local().Hour(), runOnce)

		if (dt.Local().Hour() == 20 && runOnce) || initialOnce {
			scrapURL := "https://andelenergi.dk/kundeservice/aftaler-og-priser/timepris/"
			var err error
			lowestThreeHours, lowestThreePrices, err = GetLowestPriceHours(scrapURL, runHours)
			if err != nil {
				log.Printf("Failed to get lowest price hours: %v", err)
				time.Sleep(freq)
				continue
			}
			log.Printf("Lowest price hours: %v, prices: %v", lowestThreeHours, lowestThreePrices)
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
		s, err := fetchSettings()
		if err != nil {
			log.Printf("Failed to fetch settings: %v", err)
			time.Sleep(freq)
			continue
		}

		inHoursHeating := false
		for i := 0; i < runHours; i++ {
			if lowestThreeHours[i] == dt.Local().Hour() {
				inHoursHeating = true
				break
			}
		}

		mu.Lock()
		updateNeeded := false
		if inHoursHeating || (s.DesiredDHWTemperature-r.DHWTankTopTemperature)/10 >= mustHeatTemperatureDifference {
			log.Printf("Enabling hot water: DesiredDHWTemperature=%d, Actual=%d, Paused=%v",
				s.DesiredDHWTemperature, r.DHWTankTopTemperature, s.DHWProductionPaused)
			if s.DHWProductionPaused && isAutoSavePowerMode {
				s.DHWProductionPaused = false
				s.DHWProductionPauseDuration = 0
				updateNeeded = true
			}
		} else if (s.DesiredDHWTemperature-r.DHWTankTopTemperature)/10 < stopHeatTemperatureDifference && !s.DHWProductionPaused && isAutoSavePowerMode {
			log.Printf("Pausing hot water: DesiredDHWTemperature=%d, Actual=%d, Paused=%v",
				s.DesiredDHWTemperature, r.DHWTankTopTemperature, s.DHWProductionPaused)
			s.DHWProductionPaused = true
			s.DHWProductionPauseDuration = 180
			updateNeeded = true
		}
		if updateNeeded {
			if err := updateHotWaterSettings(s); err != nil {
				log.Printf("Failed to update settings: %v", err)
			} else {
				log.Printf("Hot water production %s", map[bool]string{true: "paused", false: "enabled"}[s.DHWProductionPaused])
				c.SendSettings(nilan.Settings{
					DHWProductionPaused:        &s.DHWProductionPaused,
					DHWProductionPauseDuration: &s.DHWProductionPauseDuration,
				})
			}
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
	minValue := make([]float64, runHours)
	minHour := make([]int, runHours)
	for i := 0; i < runHours; i++ {
		minValue[i] = 9999
		minHour[i] = -1
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

		dayStr := strings.TrimSpace(str.East.Dates[len(str.East.Dates)-1].Day)
		day, err := strconv.Atoi(dayStr)
		if err != nil {
			log.Printf("Invalid day format: %s", dayStr)
			return
		}

		offset := -28
		if day == time.Now().Day()+1 && time.Now().Local().Hour() < 20 {
			offset = -52
		} else if day != time.Now().Day() && day != time.Now().Day()+1 {
			log.Printf("Price data day %d does not match current day %d or next day", day, time.Now().Day())
			return
		}

		for i := 0; i < runHours; i++ {
			for j := 0; j < 24; j++ {
				hasCompared := false
				for k := 0; k <= i; k++ {
					if minHour[k] == (j-4+24)%24 {
						hasCompared = true
						break
					}
				}
				if hasCompared {
					continue
				}

				if len(str.East.Values) <= offset+j || len(str.East.ValuesDistribution) <= offset+j {
					log.Printf("Invalid index %d for price data", offset+j)
					continue
				}

				price, err := strconv.ParseFloat(str.East.Values[offset+j], 64)
				if err != nil {
					log.Printf("Failed to parse price at index %d: %v", offset+j, err)
					continue
				}
				transport, err := strconv.ParseFloat(str.East.ValuesDistribution[offset+j], 64)
				if err != nil {
					log.Printf("Failed to parse transport at index %d: %v", offset+j, err)
					continue
				}

				totalPrice := price + transport
				if totalPrice < minValue[i] {
					minValue[i] = totalPrice
					minHour[i] = (j - 4 + 24) % 24
				}
			}
		}
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

	if minHour[0] == -1 {
		return nil, nil, fmt.Errorf("no valid price data found")
	}

	return minHour, minValue, nil
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

	log.Println("Listening at :8082...")
	if err := router.Run(":8082"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
