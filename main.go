package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
	"html/template"
	"encoding/json"
	"net/http"

	"github.com/influxdata/influxdb-client-go/v2"
	"github.com/joho/godotenv"
)

type StartJobReq struct {
	BatchSize int `json:"batch_size"`
	BatchNum int `json:"batch_number"`
	Device string `json:"device_name"`
	JobName string `json:"job_name"`
}

type Resp struct {
	Init *InitResp `json:"Init"`
	InProgress *InProgressResp `json:"InProgress"`
	BatchComplete *BatchCompleteResp `json:"BatchComplete"`
}

type InitResp struct {
	Message string `json:"message"`
}

type InProgressResp struct {
	Message string `json:"message"`
	Batch_size int `json:"batch_size"`
	Completed int `json:"completed"`
}

type BatchCompleteResp struct {
	Message string `json:"message"`
	Keyboards Keyboards `json:"keyboards"`
}

type Keyboards []struct {
	Score float64 `json:"score"`
	Keys [47]struct {
		Lower rune `json:"lower"`
		Upper rune `json:"upper"`
	} `json:"keys"`
}

type Worker struct {
	Completed int
}

type KeyboardProcessInfo struct {
	prep time.Time
	start time.Time
	end time.Time
	generations []GenerationProcessInfo
}

type GenerationProcessInfo struct {
	id int
	start time.Time
	sort time.Time
	end time.Time
}

type AppState struct {
	mu sync.Mutex
	Hosts map[string]Worker
	job_name string
	running bool
	batches int
	batch_size int
	remaining int
	keyboards Keyboards
}

type AppSyncCopy struct {
	Hosts map[string]Worker
	job_name string
	running bool
	batches int
	batch_size int
	remaining int
	keyboards Keyboards
}

func (a *AppState) sync_clone()  AppSyncCopy {
	return AppSyncCopy {
		Hosts: a.Hosts,
		job_name: a.job_name,
		running: a.running,
		batches: a.batches,
		batch_size: a.batch_size,
		remaining: a.remaining,
		keyboards: a.keyboards,
	}
}

func decode_resp(resp *http.Response) (Resp, error) {
	// decode resp as a map (only the key is needed)
	var parsedResp Resp
	err := json.NewDecoder(resp.Body).Decode(&parsedResp)
	if err != nil {
		log.Print(err)
		return Resp{}, err
	}
	return parsedResp, nil
} 

func start_worker(app *AppState, host string) {
	client := &http.Client{}
	batch_num := 0
	
	for {
		resp, err := client.Get(host + "/update")
		if err != nil {
			log.Print(err)
			return
		}
		status, err := decode_resp(resp) 
		if err != nil {
			log.Print(err)
			return
		}
		if status.InProgress != nil {
			time.Sleep(time.Second * 5)
			continue
		}

		app.mu.Lock()
		if status.BatchComplete != nil {
			batch_num += 1
			app.Hosts[host] = Worker{Completed: app.Hosts[host].Completed + 1}
			app.keyboards = append(app.keyboards, status.BatchComplete.Keyboards...)
		}
		if app.remaining == 0 {
			app.mu.Unlock()
			log.Print("Job Done, exiting thread")
			return
		}
		body := StartJobReq {
			BatchSize: app.batch_size,
			BatchNum: batch_num,
			Device: host,
			JobName: app.job_name,
		}
		app.remaining -= 1
		app.mu.Unlock()

		bodyBytes, err := json.Marshal(&body)
		if err != nil {
			log.Print(err)
			return
		}

		reader := bytes.NewReader(bodyBytes)

		client.Post(host + "/new", "application/json", reader)
	}
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("invalid .env file")
	}

	app := AppState {
		Hosts: make(map[string]Worker),
		job_name: "",
		running: false,
		batches: 0,
		batch_size: 0,
		remaining: 0,
	}

	root := func (w http.ResponseWriter, r *http.Request) {
		log.Print("Root")
		app.mu.Lock()
		defer app.mu.Unlock()

		tmpl := template.Must(template.ParseFiles("index.html"))
		tmpl.Execute(w, app.sync_clone())
	}

	add_server := func (w http.ResponseWriter, r *http.Request) {
		log.Print("Add Server")
		app.mu.Lock()
		defer app.mu.Unlock()

		host := r.PostFormValue("host")

		// TODO: check if host works

		app.Hosts[host] = Worker{Completed: 0}
		tmpl := template.Must(template.ParseFiles("index.html"))
		tmpl.ExecuteTemplate(w, "status", app.sync_clone())
	}

	start_job := func (w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		job_name := r.PostFormValue("job-name")
		batch_size, err := strconv.Atoi(r.PostFormValue("batch-size"))
		if err != nil {
			tmpl := template.Must(template.ParseFiles("fragments.html"))
			tmpl.ExecuteTemplate(w, "error", err) 
			return
		}
		batches, err := strconv.Atoi(r.PostFormValue("batches"))
		if err != nil {
			tmpl := template.Must(template.ParseFiles("fragments.html"))
			tmpl.ExecuteTemplate(w, "error", err) 
			return
		}
		app.job_name = job_name
		app.batches = batches
		app.batch_size = batch_size
		app.remaining = batches

		for host := range app.Hosts {
			go start_worker(&app, host)
		}

		tmpl := template.Must(template.ParseFiles("index.html"))
		tmpl.ExecuteTemplate(w, "status", app.sync_clone())
	}

	update := func (w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		tmpl := template.Must(template.ParseFiles("index.html"))
		tmpl.ExecuteTemplate(w, "status", app.sync_clone())
	}

	update_current_job := func (w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		url := os.Getenv("URL")
		key := os.Getenv("KEY")
		org := os.Getenv("ORG")

		client := influxdb2.NewClient(url, key)
		defer client.Close()

		api := client.QueryAPI(org)

		query := fmt.Sprintf(`from(bucket:"keyboard_gen")
			|> range(start: -1d)
			|> filter(fn: (r) => r._measurement == "%s")
			|> filter(fn: (r) => r._field == "keyboard status")`,
			app.job_name,
		)
		result, err := api.Query(context.Background(), query)
		if err != nil {
			log.Print(err)
			return
		}
		if result.Err() != nil {
		    log.Print(fmt.Sprintf("query parsing error: %s\n", result.Err().Error()))
		}
		count_start := make(map[string]int)
		count_end := make(map[string]int)
		for result.Next() {
			log.Print(result.Record().Values())
			curr_dev := result.Record().Values()["device"].(string)
			if _, ok := count_start[curr_dev]; ok == false {
				count_start[curr_dev] = 0
			}
			if _, ok := count_end[curr_dev]; ok == false {
				count_end[curr_dev] = 0
			}
			status := result.Record().Values()["_value"]
			if status != nil {
				if status == "start" {
					count_start[curr_dev] += 1
				} else if status == "end" {
					count_end[curr_dev] += 1
				}
			}
		}
		if len(count_start) > 0 {
			log.Print("")
			log.Print("keyboards started")
			for k, v := range count_start {
				if v > 0 {
					log.Print("key: " + k + " val: " + strconv.Itoa(v))
				}
			}
		}
		if len(count_end) > 0 {
			log.Print("")
			log.Print("keyboards ended")
			for k, v := range count_end {
				if v > 0 {
					log.Print("key: " + k + " val: " + strconv.Itoa(v))
				}
			}
		}

		tmpl := template.Must(template.ParseFiles("index.html"))
		tmpl.ExecuteTemplate(w, "status", app.sync_clone())
	}

	http.HandleFunc("/", root)
	http.HandleFunc("/add-server", add_server)
	http.HandleFunc("/start-job", start_job)
	http.HandleFunc("/update", update)
	http.HandleFunc("/update-job", update_current_job)

	log.Fatal(http.ListenAndServe(":8000", nil))
}

