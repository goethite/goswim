/*
Copyright 2018 Graham Lee Bevan <graham.bevan@ntlworld.com>

This file is part of gostint.

gostint is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

gostint is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with gostint.  If not, see <https://www.gnu.org/licenses/>.
*/

package job

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gbevan/gostint/jobqueues"
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/go-chi/chi"
	"github.com/go-chi/render"
	"github.com/hashicorp/vault/api"
)

const notfound = "not found"

// JobRouter holds config state, e.g. the handle for the database
type JobRouter struct { // nolint
	Db *mgo.Database
}

var jobRouter JobRouter

// JobRequest localises jobqueues.Job in this module
type JobRequest jobqueues.Job // nolint

// Bind Binder of decoded request payload
func (j *JobRequest) Bind(req *http.Request) error {
	j.Qname = strings.ToLower(j.Qname)
	j.Status = "queued"
	j.Submitted = time.Now()

	return nil
}

// Routes Route handlers for jobs
func Routes(db *mgo.Database) *chi.Mux {
	jobRouter = JobRouter{
		Db: db,
	}
	router := chi.NewRouter()

	router.Post("/", postJob)
	router.Post("/kill/{jobID}", killJob)
	router.Get("/{jobID}", getJob)
	router.Delete("/{jobID}", deleteJob)

	return router
}

// ErrResponse struct for http error responses
type ErrResponse struct {
	Err            error `json:"-"` // low-level runtime error
	HTTPStatusCode int   `json:"-"` // http response status code

	StatusText string `json:"status"`          // user-level status message
	AppCode    int64  `json:"code,omitempty"`  // application-specific error code
	ErrorText  string `json:"error,omitempty"` // application-level error message, for debugging
}

// Render to render a http return code
func (e *ErrResponse) Render(w http.ResponseWriter, r *http.Request) error {
	render.Status(r, e.HTTPStatusCode)
	return nil
}

// ErrInvalidRequest return an invalid http request
func ErrInvalidRequest(err error) render.Renderer {
	return &ErrResponse{
		Err:            err,
		HTTPStatusCode: 400,
		StatusText:     "Invalid job request.",
		ErrorText:      err.Error(),
	}
}

// ErrNotFound return a not found error
func ErrNotFound(err error) render.Renderer {
	return &ErrResponse{
		Err:            err,
		HTTPStatusCode: 404,
		StatusText:     "Not Found.",
		ErrorText:      err.Error(),
	}
}

// ErrInternalError return internal error
func ErrInternalError(err error) render.Renderer {
	return &ErrResponse{
		Err:            err,
		HTTPStatusCode: 500,
		StatusText:     "Internal Error.",
		ErrorText:      err.Error(),
	}
}

type getResponse struct {
	ID             string    `json:"_id"`
	Status         string    `json:"status"`
	NodeUUID       string    `json:"node_uuid"`
	Qname          string    `json:"qname"`
	ContainerImage string    `json:"container_image"`
	Submitted      time.Time `json:"submitted"`
	Started        time.Time `json:"started"`
	Ended          time.Time `json:"ended"`
	Output         string    `json:"output"`
	ReturnCode     int       `json:"return_code"`
}

// AuthCtxKey context key for authentication state & policy map
type AuthCtxKey string

func getJob(w http.ResponseWriter, req *http.Request) {
	// ctx := req.Context()
	// log.Printf("Context: %v", ctx.Value(AuthCtxKey("auth")))
	jobID := strings.TrimSpace(chi.URLParam(req, "jobID"))
	if jobID == "" {
		render.Render(w, req, ErrInvalidRequest(errors.New("job ID missing from GET path")))
		return
	}
	if !bson.IsObjectIdHex(jobID) {
		render.Render(w, req, ErrInvalidRequest(errors.New("Invalid job ID (not ObjectIdHex)")))
		return
	}
	coll := jobRouter.Db.C("queues")
	var job JobRequest
	err := coll.FindId(bson.ObjectIdHex(jobID)).One(&job)
	if err != nil {
		if err.Error() == notfound {
			render.Render(w, req, ErrNotFound(err))
			return
		}
		render.Render(w, req, ErrInternalError(err))
		return
	}
	render.JSON(w, req, getResponse{
		ID:             job.ID.Hex(),
		Status:         job.Status,
		NodeUUID:       job.NodeUUID,
		Qname:          job.Qname,
		ContainerImage: job.ContainerImage,
		Submitted:      job.Submitted,
		Started:        job.Started,
		Ended:          job.Ended,
		Output:         job.Output,
		ReturnCode:     job.ReturnCode,
	})
}

type deleteResponse struct {
	ID string `json:"_id"`
}

func deleteJob(w http.ResponseWriter, req *http.Request) {
	jobID := strings.TrimSpace(chi.URLParam(req, "jobID"))
	if jobID == "" {
		render.Render(w, req, ErrInvalidRequest(errors.New("job ID missing from GET path")))
		return
	}
	if !bson.IsObjectIdHex(jobID) {
		render.Render(w, req, ErrInvalidRequest(errors.New("Invalid job ID (not ObjectIdHex)")))
		return
	}
	coll := jobRouter.Db.C("queues")

	// Get status and ensure job is not running/stopping
	// TODO: Look at making the find and remove atomic
	var job jobqueues.Job
	err := coll.FindId(bson.ObjectIdHex(jobID)).One(&job)
	if err != nil {
		if err.Error() == notfound {
			render.Render(w, req, ErrNotFound(err))
			return
		}
		render.Render(w, req, ErrInternalError(err))
		return
	}

	if job.Status == "running" || job.Status == "stopping" {
		render.Render(w, req, ErrInvalidRequest(errors.New("Cannot delete a running/stopping job")))
		return
	}

	err = coll.RemoveId(bson.ObjectIdHex(jobID))
	if err != nil {
		if err.Error() == notfound {
			render.Render(w, req, ErrNotFound(err))
			return
		}
		render.Render(w, req, ErrInternalError(err))
		return
	}
	render.JSON(w, req, deleteResponse{
		ID: jobID,
	})
}

type postResponse struct {
	ID     string `json:"_id"`
	Status string `json:"status"`
	Qname  string `json:"qname"`
}

// postJob post a job to the fifo queue
// curl http://127.0.0.1:3232/v1/api/job -X POST -d '{"qname":"play", "jobtype": "ansible", "content": "base64 here", "run": "hello.yml"}'
// -> {"qname":"play","jobtype":"ansible","content":"base64 here","run":"hello.yml"}
func postJob(w http.ResponseWriter, req *http.Request) {
	data := &JobRequest{}
	if err := render.Bind(req, data); err != nil {
		render.Render(w, req, ErrInvalidRequest(err))
		return
	}
	job := data

	coll := jobRouter.Db.C("queues")
	newID := bson.NewObjectId()
	jobRequest := job
	jobRequest.ID = newID

	if jobRequest.WrapSecretID == "" {
		render.Render(w, req, ErrInvalidRequest(errors.New("AppRole SecretID's Wrapping Token must be present in the job request")))
		return
	}

	// get encrypted payload from cubbyhole
	client, err := api.NewClient(&api.Config{
		Address: os.Getenv("VAULT_ADDR"),
	})
	if err != nil {
		render.Render(w, req, ErrInternalError(fmt.Errorf("Failed create vault client api: %s", err)))
		return
	}
	client.SetToken(job.CubbyToken)
	resp, err := client.Logical().Read(job.CubbyPath)
	if err != nil {
		render.Render(w, req, ErrInternalError(fmt.Errorf("Failed to read cubbyhole from vault: %s", err)))
		return
	}
	job.Payload = resp.Data["payload"].(string)

	err = coll.Insert(jobRequest)
	if err != nil {
		panic(err)
	}

	render.JSON(w, req, postResponse{
		ID:     jobRequest.ID.Hex(),
		Status: jobRequest.Status,
		Qname:  jobRequest.Qname,
	})
}

type killResponse struct {
	ID            string `json:"_id"`
	ContainerID   string `json:"container_id"`
	Status        string `json:"status"`
	KillRequested bool   `json:"kill_requested"`
}

func killJob(w http.ResponseWriter, req *http.Request) {
	jobID := strings.TrimSpace(chi.URLParam(req, "jobID"))
	log.Printf("killJob ID: %s", jobID)
	if jobID == "" {
		render.Render(w, req, ErrInvalidRequest(errors.New("job ID missing from GET path")))
		return
	}
	if !bson.IsObjectIdHex(jobID) {
		render.Render(w, req, ErrInvalidRequest(errors.New("Invalid job ID (not ObjectIdHex)")))
		return
	}
	coll := jobRouter.Db.C("queues")
	var job jobqueues.Job
	err := coll.FindId(bson.ObjectIdHex(jobID)).One(&job)
	if err != nil {
		if err.Error() == notfound {
			render.Render(w, req, ErrNotFound(err))
			return
		}
		render.Render(w, req, ErrInternalError(err))
		return
	}

	// flag job to be killed - we cant do this directly here because this
	// instance of gostint may not be the same one that is running the job
	job.UpdateJob(bson.M{
		"kill_requested": true,
	})

	render.JSON(w, req, killResponse{
		ID:            job.ID.Hex(),
		ContainerID:   job.ContainerID,
		Status:        job.Status,
		KillRequested: true,
	})
}
