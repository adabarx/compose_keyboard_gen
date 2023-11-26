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
	batch_size int
	job_name string
}

type UpdateRespSuccess struct {
	Success any
}

type UpdateRespInit struct {
	NoBatch any
}

type UpdateRespInProc struct {
	BatchInProgress any
}

type AppState struct {
	mu sync.Mutex
	hosts map[int]string
	job_name string
	running bool
	total int
	batch_size int
	remaining int
}

type AppSyncCopy struct {
	hosts map[int]string
	job_name string
	running bool
	total int
	batch_size int
	remaining int
}

func (a *AppState) sync_clone()  AppSyncCopy {
	return AppSyncCopy {
		hosts: a.hosts,
		job_name: a.job_name,
		running: a.running,
		total: a.total,
		batch_size: a.batch_size,
		remaining: a.remaining,
	}
}

func decode_resp(resp *http.Response) (string, error) {
	var nb UpdateRespInit
	var ip UpdateRespInProc
	var su UpdateRespSuccess

	// TODO: there has to be a better way.
	err := json.NewDecoder(resp.Body).Decode(&nb)
	if err != nil {
		err := json.NewDecoder(resp.Body).Decode(&ip)
		if err != nil {
			err := json.NewDecoder(resp.Body).Decode(&su)
			if err != nil {
				return "", err
			}
			return "success", nil
		}
		return "in progress", nil
	}
	return "available", nil
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
		if status == "in progress" {
			time.Sleep(time.Second * 5)
			continue
		}
		if status == "success" {
			// gather data
		}

		app.mu.Lock()
		if app.remaining == 0 {
			app.mu.Unlock()
			log.Print("Job Done, exiting thread")
			return
		}
		body := StartJobReq {
			batch_size: 0,
			job_name: app.job_name,
		}
		if app.batch_size > app.remaining {
			body.batch_size = app.remaining
		} else {
			body.batch_size = app.batch_size
		}
		app.remaining -= body.batch_size
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
		hosts: make(map[int]string),
		job_name: "",
		running: false,
		total: 0,
		batch_size: 0,
		remaining: 0,
	}

	root := func (w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		tmpl := template.Must(template.ParseFiles("index.html"))
		tmpl.Execute(w, app.sync_clone())
	}

	add_server := func (w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()

		host := r.PostFormValue("host")

		// check if host works

		app.hosts[len(app.hosts)] = host
		tmpl := template.Must(template.ParseFiles("index.html"))
		tmpl.ExecuteTemplate(w, "server-status", app.sync_clone())
	}

	start_job := func (w http.ResponseWriter, r *http.Request) {
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

		// TODO: spawn go routines to start and manage each worker
		for _, host := range app.hosts {
			go start_worker(&app, host)
		}

		tmpl := template.Must(template.ParseFiles("job.html"))
		tmpl.Execute(w, app.sync_clone())
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

		tmpl := template.Must(template.ParseFiles("index.html"))
		tmpl.ExecuteTemplate(w, "job-status", app.sync_clone()) // map of server jobs
	}

	http.HandleFunc("/", root)
	http.HandleFunc("/add-server", add_server)
	http.HandleFunc("/start-job", start_job)
	http.HandleFunc("/update-job", update_current_job)

	log.Fatal(http.ListenAndServe(":8000", nil))
}

