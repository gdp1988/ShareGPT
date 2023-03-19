package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"github.com/acheong08/ShareGPT/checks"
	"github.com/acheong08/ShareGPT/typings"
	gin "github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
)

var (
	jar     = tls_client.NewCookieJar()
	options = []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(360),
		tls_client.WithClientProfile(tls_client.Chrome_110),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithCookieJar(jar), // create cookieJar instance and pass it as argument
	}
	client, _ = tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
)

var (
	redisAddr = os.Getenv("REDIS_ADDRESS")
	redisPass = os.Getenv("REDIS_PASSWORD")
)

var rdb *redis.Client

func init() {
	if redisAddr == "" {
		panic("REDIS_ADDRESS is not set")
	}
	rdb = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPass,
		DB:       0,
	})
}

func cors(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

func main() {
	router := gin.Default()
	router.Use(cors)
	router.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})
	// OPTIONS any route
	router.OPTIONS("/*path", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "ok",
		})
	})
	router.POST("/api_key/submit", func(c *gin.Context) {
		var api_key typings.APIKeySubmission
		c.BindJSON(&api_key)
		if api_key.APIKey == "" {
			c.JSON(400, gin.H{
				"error": "API key is empty",
			})
			return
		}
		// Check if API key is valid
		creditSummary, err := checks.GetCredits(api_key.APIKey)
		if err != nil {
			c.JSON(500, gin.H{
				"error": err.Error(),
			})
			return
		}
		if creditSummary.HardLimitUSD < 1 {
			c.JSON(400, gin.H{
				"error": "Not enough credits",
			})
			return
		}
		// Return credit summary
		c.JSON(200, creditSummary)
		// Save to Redis
		go func(creditSummary typings.BillingSubscription) {
			// Save to Redis without expiration
			err := rdb.Set(api_key.APIKey, creditSummary.HardLimitUSD, 0).Err()
			if err != nil {
				println(fmt.Errorf("error saving to Redis: %v", err))
			}
		}(creditSummary)
	})
	router.POST("/api_key/delete", func(c *gin.Context) {
		// Delete API key from Redis
		var api_key typings.APIKeySubmission
		err := c.BindJSON(&api_key)
		if err != nil {
			c.JSON(500, gin.H{
				"error": "Error binding JSON",
			})
			return
		}
		if api_key.APIKey == "" {
			c.JSON(400, gin.H{
				"error": "API key is empty",
			})
			return
		}
		err = rdb.Del(api_key.APIKey).Err()
		if err != nil {
			c.JSON(500, gin.H{
				"error": err.Error(),
			})
			return
		}
		c.JSON(200, gin.H{
			"message": "API key deleted",
		})
	})
	router.POST("/v1/chat", proxy)
	router.Run()
}

func proxy(c *gin.Context) {

	var url string
	var err error
	var request_method string
	var request *http.Request
	var response *http.Response

	url = "https://api.openai.com/v1/chat/completions"
	request_method = c.Request.Method

	var body []byte
	body, err = io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	// Convert to JSON
	var jsonBody map[string]interface{}
	err = json.Unmarshal(body, &jsonBody)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	// Set model to `gpt-3.5-turbo`
	jsonBody["model"] = "gpt-3.5-turbo"
	// Convert back to bytes
	body, err = json.Marshal(jsonBody)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	request, err = http.NewRequest(request_method, url, bytes.NewReader(body))
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	request.Header.Set("Host", "api.openai.com")
	request.Header.Set("Origin", "https://platform.openai.com/playground")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "keep-alive")
	request.Header.Set("Keep-Alive", "timeout=360")
	request.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/87.0.4280.88 Safari/537.36")
	// Authorization
	var authorization string
	if c.Request.Header.Get("Authorization") == "" {
		// Set authorization header from Redis
		random_key, err := rdb.RandomKey().Result()
		if err != nil {
			c.JSON(500, gin.H{"error": "Failed to get random key from Redis"})
			println(err.Error())
			return
		}
		counter := 0
		for {
			if counter > 5 {
				c.JSON(500, gin.H{"error": "Failed to get valid key from Redis"})
				return
			}
			// Check credit
			creditSummary, err := checks.GetCredits(random_key)
			if err != nil {
				c.JSON(500, gin.H{"error": "OpenAI failed"})
				return
			}
			if creditSummary.HardLimitUSD < 1 {
				c.JSON(500, gin.H{
					"error": "Not enough credits",
				})
				// Remove key from Redis
				err = rdb.Del(random_key).Err()
				if err != nil {
					println(fmt.Errorf("error deleting key from Redis: %v", err))
				}
				counter += 1
				continue
			}
			break
		}

		authorization = "Bearer " + random_key
	} else {
		// Set authorization header from request
		authorization = c.Request.Header.Get("Authorization")
	}
	request.Header.Set("Authorization", authorization)

	response, err = client.Do(request)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer response.Body.Close()
	c.Header("Content-Type", response.Header.Get("Content-Type"))
	// Make line reader
	lr := NewLineReader(response.Body, func(line string) bool {
		return true
	})
	// Get status code
	c.Status(response.StatusCode)
	c.Stream(func(w io.Writer) bool {
		// Write data to client using LineReader
		io.Copy(w, lr)
		return false
	})

}

type LineReader struct {
	reader  io.Reader
	onLine  func(line string) bool
	scanner *bufio.Scanner
}

func NewLineReader(reader io.Reader, onLine func(line string) bool) *LineReader {
	return &LineReader{
		reader:  reader,
		onLine:  onLine,
		scanner: bufio.NewScanner(reader),
	}
}

func (lr *LineReader) Read(p []byte) (int, error) {
	if lr.scanner.Scan() {
		line := lr.scanner.Text()
		if lr.onLine(line) {
			line += "\n" // Append newline character
			n := copy(p, line)
			return n, nil
		} else {
			return 0, io.EOF
		}
	}
	if err := lr.scanner.Err(); err != nil {
		return 0, err
	}
	return 0, io.EOF
}
