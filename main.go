package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/influxdata/influxdb-client-go/v2"
	"github.com/joho/godotenv"
)

type StartJobReq struct {
	BatchSize int `json:"batch_size"`
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

type AppState struct {
	mu sync.Mutex
	Hosts map[string]Worker
	job_name string
	running bool
	total int
	batch_size int
	remaining int
	keyboards Keyboards
}

type AppSyncCopy struct {
	Hosts map[string]Worker
	job_name string
	running bool
	total int
	batch_size int
	remaining int
}

func (a *AppState) sync_clone()  AppSyncCopy {
	return AppSyncCopy {
		Hosts: a.Hosts,
		job_name: a.job_name,
		running: a.running,
		total: a.total,
		batch_size: a.batch_size,
		remaining: a.remaining,
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
			app.Hosts[host] = Worker{Completed: app.Hosts[host].Completed + 1}
			app.keyboards = append(app.keyboards, status.BatchComplete.Keyboards...)
		}
		if app.remaining == 0 {
			app.mu.Unlock()
			log.Print("Job Done, exiting thread")
			return
		}
		body := StartJobReq {
			BatchSize: 0,
			JobName: app.job_name,
		}
		if app.batch_size > app.remaining {
			body.BatchSize = app.remaining
		} else {
			body.BatchSize = app.batch_size
		}
		app.remaining -= body.BatchSize
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
		total: 0,
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
		tmpl := template.Must(template.ParseFiles("fragments.html"))
		tmpl.ExecuteTemplate(w, "server-status", app.sync_clone())
	}

	start_job := func (w http.ResponseWriter, r *http.Request) {
		log.Print("Start Job")
		app.mu.Lock()
		defer app.mu.Unlock()

		job_name := r.PostFormValue("job-name")
		total, err := strconv.Atoi(r.PostFormValue("total"))
		if err != nil {
			// TODO: return error html
			log.Print("need a number")
		}
		app.job_name = job_name
		app.total = total
		app.batch_size = int(math.Round(math.Sqrt(float64(total))))
		app.remaining = total

		log.Print("before threads")
		// TODO: spawn go routines to start and manage each worker
		for host := range app.Hosts {
			go start_worker(&app, host)
		}

		tmpl := template.Must(template.ParseFiles("fragments.html"))
		tmpl.ExecuteTemplate(w, "server-status", app.sync_clone())
		log.Print("after template")
	}

	update := func (w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		tmpl := template.Must(template.ParseFiles("fragments.html"))
		tmpl.ExecuteTemplate(w, "server-status", app.sync_clone())
	}

	update_current_job := func (w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		url := os.Getenv("URL")
		key := os.Getenv("KEY")
		org := os.Getenv("ORG")

		client := influxdb2.NewClient(url, key)
		api := client.QueryAPI(org)
		query := fmt.Sprintf(`from(bucket:"keyboard_gen")
			|> filter(fn: (r) => r._measurement == "%s")`,
			app.job_name,
		)
		results, err := api.Query(context.Background(), query)
		if err != nil {
			log.Print("error querying influx")
		}
		fmt.Printf("results: %v\n", results)

		tmpl := template.Must(template.ParseFiles("fragments.html"))
		tmpl.ExecuteTemplate(w, "job-status", app.sync_clone()) // map of server jobs
	}

	http.HandleFunc("/", root)
	http.HandleFunc("/add-server", add_server)
	http.HandleFunc("/start-job", start_job)
	http.HandleFunc("/update", update)
	http.HandleFunc("/update-job", update_current_job)

	log.Fatal(http.ListenAndServe(":8000", nil))
}

