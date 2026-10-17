//go:build browser

package browser

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The App Engine forms of Cloud Tasks and Cloud Scheduler (#1447), in the
// storage shard: each change made in the browser is read back through the
// official client. This instance has no App Engine, so nothing is sent
// anywhere: the queue is paused before its task is made, and the job's
// schedule is once a year.
const (
	envTasks     = "CLOUDBURROW_TEST_TASKS"
	envScheduler = "CLOUDBURROW_TEST_SCHEDULER"
)

// appEngineAddr is an emulator's address from env, or a skip when it is unset.
func appEngineAddr(t *testing.T, env string) string {
	t.Helper()
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(env)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the official client", env)
	}
	return addr
}

func appEngineClientOpts(addr string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// TestTasksAppEngineRoutingAndTaskThroughTheForms: Edit queue's App Engine
// routing override fields, empty on a new queue, are saved and
// cloud.google.com/go/cloudtasks/apiv2's GetQueue reads the override with its
// computed host; Create task with the App Engine target makes a task
// ListTasks returns with its appEngineHttpRequest, the queue's override
// applied to its routing.
func TestTasksAppEngineRoutingAndTaskThroughTheForms(t *testing.T) {
	needService(t, "tasks")
	ctx := context.Background()
	c, err := cloudtasks.NewClient(ctx, appEngineClientOpts(appEngineAddr(t, envTasks))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/us-central1"
	name := parent + "/queues/browser-ae"
	if _, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: parent, Queue: &taskspb.Queue{Name: name}}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteQueue(context.Background(), &taskspb.DeleteQueueRequest{Name: name}) })
	// Paused, so the task made below is never attempted.
	if _, err := c.PauseQueue(ctx, &taskspb.PauseQueueRequest{Name: name}); err != nil {
		t.Fatalf("PauseQueue: %v", err)
	}

	p.navigate("/tasks/queues?project=" + project)
	p.clickText("#view tbody a", name)
	p.clickText("#view .page-actions button", "Edit queue")
	p.waitFor(`document.querySelector(".modal.is-open #f-appEngineService") !== null`)
	var empty []string
	p.eval(`["appEngineService", "appEngineVersion", "appEngineInstance"].map((f) => document.querySelector(".modal.is-open #f-" + f).value)`, &empty)
	if strings.Join(empty, "") != "" {
		t.Errorf("Edit queue's App Engine routing override opens with %q on a queue with none", empty)
	}
	p.setField(".modal.is-open #f-appEngineService", "worker")
	p.setField(".modal.is-open #f-appEngineVersion", "v1")
	p.setField(".modal.is-open #f-appEngineInstance", "i1")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	got, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if o := got.GetAppEngineRoutingOverride(); o.GetService() != "worker" || o.GetVersion() != "v1" || o.GetInstance() != "i1" ||
		o.GetHost() != "i1.v1.worker."+project+".appspot.com" {
		t.Errorf("after Edit queue, GetQueue reads the override %v; want worker, v1, i1 and its host", o)
	}
	if got.GetState() != taskspb.Queue_PAUSED {
		t.Errorf("Edit queue changed the queue's state to %v; want it still paused", got.GetState())
	}

	p.clickText("#view .page-actions button", "Create task")
	p.waitFor(`document.querySelector(".modal.is-open #f-target") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-target");
		f.value = "App Engine"; f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.setField(".modal.is-open #f-relativeUri", "/work?from=browser")
	p.setField(".modal.is-open #f-body", "hello")
	p.setField(".modal.is-open #f-taskId", "ae-task")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPost, "/api/actions/tasks"); len(sent) != 1 {
		t.Errorf("Create task sent %d actions, want one: %v", len(sent), sent)
	}

	it := c.ListTasks(ctx, &taskspb.ListTasksRequest{Parent: name, ResponseView: taskspb.Task_FULL})
	var tasks []*taskspb.Task
	for {
		task, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		tasks = append(tasks, task)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasks returns %d tasks after Create task, want one: %v", len(tasks), tasks)
	}
	r := tasks[0].GetAppEngineHttpRequest()
	if tasks[0].GetName() != name+"/tasks/ae-task" || r == nil || r.GetRelativeUri() != "/work?from=browser" ||
		r.GetHttpMethod() != taskspb.HttpMethod_POST || string(r.GetBody()) != "hello" ||
		r.GetAppEngineRouting().GetHost() != "i1.v1.worker."+project+".appspot.com" {
		t.Errorf("ListTasks reads %v; want the App Engine task the form made, routed by the queue's override", tasks[0])
	}
}

// TestSchedulerCreateAppEngineJobThroughTheForm: Create job with an App
// Engine relative URI, service and version makes a job
// cloud.google.com/go/scheduler/apiv1's GetJob reads with an
// appEngineHttpTarget, POST, the body and the routing's computed host; a
// relative URI not beginning with / is refused by the form's pattern and
// nothing is posted.
func TestSchedulerCreateAppEngineJobThroughTheForm(t *testing.T) {
	needService(t, "scheduler")
	ctx := context.Background()
	c, err := scheduler.NewCloudSchedulerClient(ctx, appEngineClientOpts(appEngineAddr(t, envScheduler))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/us-central1/jobs/browser-ae"
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: name}) })

	p.navigate("/scheduler/jobs/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-appEngineRelativeUri") !== null`)
	p.setField("#view #f-name", "browser-ae")
	p.setField("#view #f-schedule", "0 0 1 1 *")
	p.setField("#view #f-body", "tick")
	p.setField("#view #f-appEngineRelativeUri", "cron/no-slash")
	p.setField("#view #f-appEngineService", "cron")
	p.setField("#view #f-appEngineVersion", "v2")
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view #f-appEngineRelativeUri").getAttribute("aria-invalid") === "true"`)
	if posts := p.posts("/api/resources/scheduler"); len(posts) != 0 {
		t.Fatalf("a relative URI not beginning with / was posted: %v", posts)
	}

	p.setField("#view #f-appEngineRelativeUri", "/cron/tick")
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`location.pathname === "/scheduler/jobs" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser-ae"))`)
	if posts := p.posts("/api/resources/scheduler"); len(posts) != 1 {
		t.Errorf("the create was posted %d times, want once: %v", len(posts), posts)
	}
	got, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	a := got.GetAppEngineHttpTarget()
	if a == nil || a.GetRelativeUri() != "/cron/tick" || a.GetHttpMethod() != schedulerpb.HttpMethod_POST ||
		string(a.GetBody()) != "tick" || a.GetAppEngineRouting().GetService() != "cron" ||
		a.GetAppEngineRouting().GetVersion() != "v2" || a.GetAppEngineRouting().GetHost() != "v2.cron."+project+".appspot.com" ||
		got.GetSchedule() != "0 0 1 1 *" {
		t.Errorf("GetJob reads %v; want the App Engine job the form made", got)
	}

	// The job's page names its target App Engine.
	p.clickText("#view tbody a", name)
	p.waitFor(`document.querySelector("#view").textContent.includes("App Engine")`)
	if code, body := consoleDo(t, http.MethodGet, "/api/detail/scheduler?project="+project+"&name="+url.QueryEscape(name), ""); code != http.StatusOK ||
		!strings.Contains(body, "App Engine") {
		t.Errorf("the console API's page for the job = %d: %s; want its App Engine target", code, body)
	}
}
