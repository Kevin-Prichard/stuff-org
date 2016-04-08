// stuff store. Backed by a database.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Component struct {
	Id            int    `json:"id"`
	Equiv_set     int    `json:"equiv_set,omitempty"`
	Value         string `json:"value"`
	Category      string `json:"category"`
	Description   string `json:"description"`
	Quantity      string `json:"quantity"` // at this point just a string.
	Notes         string `json:"notes,omitempty"`
	Datasheet_url string `json:"datasheet_url,omitempty"`
	Drawersize    int    `json:"drawersize,omitempty"`
	Footprint     string `json:"footprint,omitempty"`
}

// Some useful pre-defined set of categories
var available_category []string = []string{
	"Resistor", "Potentiometer", "R-Network",
	"Capacitor (C)", "Aluminum Cap", "Inductor (L)",
	"Diode (D)", "Power Diode", "LED",
	"Transistor", "Mosfet", "IGBT",
	"Integrated Circuit (IC)", "IC Analog", "IC Digital",
	"Connector", "Socket", "Switch",
	"Fuse", "Mounting", "Heat Sink",
	"Microphone", "Transformer", "? MYSTERY",
}

// Modify a user pointer. Returns 'true' if the changes should be commited.
type ModifyFun func(comp *Component) bool

// Interface to our storage backend.
type StuffStore interface {
	// Find a component by its ID. Returns nil if it does not exist. Don't
	// modify the returned pointer.
	FindById(id int) *Component

	// Edit record of given ID. If ID is new, it is inserted and an empty
	// record returned to be edited.
	// Returns if record has been saved, possibly with message.
	// This does _not_ influence the equivalence set settings, use
	// the JoinSet()/LeaveSet() functions for that.
	EditRecord(id int, updater ModifyFun) (bool, string)

	// Have component with id join set with given ID.
	JoinSet(id int, equiv_set int)

	// Leave any set we are in and go back to the default set
	// (which is equiv_set == id)
	LeaveSet(id int)

	// Get possible matching components of given component,
	// including all the components that are in the sets the matches
	// are in.
	// Ordered by equivalence set, id.
	MatchingEquivSetForComponent(component int) []*Component

	// Given a search term, returns all the components that match, ordered
	// by some internal scoring system. Don't modify the returned objects!
	Search(search_term string) []*Component
}

var wantTimings = flag.Bool("want-timings", false, "Print processing timings.")

func ElapsedPrint(msg string, start time.Time) {
	if *wantTimings {
		log.Printf("%s took %s", msg, time.Since(start))
	}
}

var cache_templates = flag.Bool("cache-templates", true,
	"Cache templates. False for online editing.")
var templates = template.Must(template.ParseFiles(
	"template/form-template.html",
	"template/status-table.html",
	"template/set-drag-drop.html",
	"template/category-Diode.svg",
	"template/category-LED.svg",
	"template/category-Capacitor.svg",
	"template/4-Band_Resistor.svg",
	"template/5-Band_Resistor.svg",
	"template/package-TO-39.svg",
	"template/package-TO-220.svg",
	"template/package-DIP-14.svg",
	"template/package-DIP-16.svg",
	"template/package-DIP-28.svg"))

func setContentTypeFromTemplateName(template_name string, header http.Header) {
	switch {
	case strings.HasSuffix(template_name, ".svg"):
		header.Set("Content-Type", "image/svg+xml")
	default:
		header.Set("Content-Type", "text/html; charset=utf-8")
	}
}

// for now, render templates directly to easier edit them.
func renderTemplate(w io.Writer, header http.Header, template_name string, p interface{}) bool {
	var err error
	if *cache_templates {
		template := templates.Lookup(template_name)
		if template == nil {
			return false
		}
		setContentTypeFromTemplateName(template_name, header)
		err = template.Execute(w, p)
	} else {
		t, err := template.ParseFiles("template/" + template_name)
		if err != nil {
			log.Printf("Template broken %s %s", template_name, err)
			return false
		}
		setContentTypeFromTemplateName(template_name, header)
		err = t.Execute(w, p)
	}
	if err != nil {
		log.Printf("Template broken %s", template_name)
		return false
	}
	return true
}

func sendResource(local_path string, fallback_resource string, out http.ResponseWriter) {
	cache_time := 900
	header_addon := ""
	content, _ := ioutil.ReadFile(local_path)
	if content == nil && fallback_resource != "" {
		local_path = fallback_resource
		content, _ = ioutil.ReadFile(local_path)
		cache_time = 10 // fallbacks might change more often.
		header_addon = ",must-revalidate"
	}
	out.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d%s", cache_time, header_addon))
	switch {
	case strings.HasSuffix(local_path, ".png"):
		out.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(local_path, ".css"):
		out.Header().Set("Content-Type", "text/css")
	case strings.HasSuffix(local_path, ".svg"):
		out.Header().Set("Content-Type", "image/svg+xml")
	default:
		out.Header().Set("Content-Type", "image/jpg")
	}

	out.Write(content)
}

// TODO: this component image serving stuff needs to move somewhere else.

func serveComponentImage(component *Component, category string, value string,
	out http.ResponseWriter) bool {
	// If we got a category string, it takes precedence
	if len(category) == 0 && component != nil {
		category = component.Category
	}
	switch category {
	case "Resistor":
		return serveResistorImage(component, value, out)
	case "Diode (D)":
		return renderTemplate(out, out.Header(), "category-Diode.svg", component)
	case "LED":
		return renderTemplate(out, out.Header(), "category-LED.svg", component)
	case "Capacitor (C)":
		return renderTemplate(out, out.Header(), "category-Capacitor.svg", component)
	}
	return false
}

func servePackageImage(component *Component, out http.ResponseWriter) bool {
	if component == nil || component.Footprint == "" {
		return false
	}
	return renderTemplate(out, out.Header(), "package-"+component.Footprint+".svg", component)
}

func compImageServe(store StuffStore, imgPath string, staticPath string,
	out http.ResponseWriter, r *http.Request) {
	prefix_len := len("/img/")
	requested := r.URL.Path[prefix_len:]
	path := imgPath + "/" + requested + ".jpg"
	if _, err := os.Stat(path); err == nil { // we have an image.
		sendResource(path, staticPath+"/fallback.jpg", out)
		return
	}
	// No image, but let's see if we can do something from the
	// component
	if comp_id, err := strconv.Atoi(requested); err == nil {
		component := store.FindById(comp_id)
		category := r.FormValue("c") // We also allow these if available
		value := r.FormValue("v")
		if (component != nil || len(category) > 0 || len(value) > 0) &&
			serveComponentImage(component, category, value, out) {
			return
		}
		if servePackageImage(component, out) {
			return
		}
	}
	// Use fallback-resource straight away to get short cache times.
	sendResource("", staticPath+"/fallback.jpg", out)
}

func staticServe(staticPath string, out http.ResponseWriter, r *http.Request) {
	prefix_len := len("/static/")
	resource := r.URL.Path[prefix_len:]
	sendResource(staticPath+"/"+resource, "", out)
}

func stuffStoreRoot(out http.ResponseWriter, r *http.Request) {
	http.Redirect(out, r, "/form", 302)
}

func parseAllowedEditorCIDR(allowed string) []*net.IPNet {
	all_allowed := strings.Split(allowed, ",")
	allowed_nets := make([]*net.IPNet, 0, len(all_allowed))
	for i := 0; i < len(all_allowed); i++ {
		if all_allowed[i] == "" {
			continue
		}
		_, net, err := net.ParseCIDR(all_allowed[i])
		if err != nil {
			log.Fatal("--edit-permission-nets: Need IP/Network format: ", err)
		} else {
			allowed_nets = append(allowed_nets, net)
		}
	}
	return allowed_nets
}

func main() {
	imageDir := flag.String("imagedir", "img-srv", "Directory with component images")
	staticResource := flag.String("staticdir", "static",
		"Directory with static resources")
	port := flag.Int("port", 2000, "Port to serve from")
	dbFile := flag.String("dbfile", "stuff-database.db", "SQLite database file")
	logfile := flag.String("logfile", "", "Logfile to write interesting events")
	do_cleanup := flag.Bool("cleanup-db", false, "Cleanup run of database")
	permitted_nets := flag.String("edit-permission-nets", "", "Comma separated list of networks (CIDR format IP-Addr/network) that are allowed to edit content")

	flag.Parse()

	edit_nets := parseAllowedEditorCIDR(*permitted_nets)

	if *logfile != "" {
		f, err := os.OpenFile(*logfile,
			os.O_RDWR|os.O_CREATE|os.O_APPEND,
			0644)
		if err != nil {
			log.Fatalf("error opening file: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	is_dbfilenew := true
	if _, err := os.Stat(*dbFile); err == nil {
		is_dbfilenew = false
	}

	db, err := sql.Open("sqlite3", *dbFile)
	if err != nil {
		log.Fatal(err)
	}

	var store StuffStore
	store, err = NewDBBackend(db, is_dbfilenew)
	if err != nil {
		log.Fatal(err)
	}

	// Very crude way to run all the cleanup routines if
	// requested. This is the only thing we do.
	if *do_cleanup {
		for i := 0; i < 3000; i++ {
			if c := store.FindById(i); c != nil {
				store.EditRecord(i, func(c *Component) bool {
					before := *c
					cleanupCompoent(c)
					if *c == before {
						return false
					}
					json, _ := json.Marshal(before)
					log.Printf("----- %s", json)
					return true
				})
			}
		}
		return
	}

	http.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		compImageServe(store, *imageDir, *staticResource, w, r)
	})
	http.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		staticServe(*staticResource, w, r)
	})

	http.HandleFunc("/form", func(w http.ResponseWriter, r *http.Request) {
		entryFormHandler(store, *imageDir, edit_nets, w, r)
	})
	http.HandleFunc("/api/related-set", func(w http.ResponseWriter, r *http.Request) {
		relatedComponentSetOperations(store, edit_nets, w, r)
	})

	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		showSearchPage(w, r)
	})
	// Pre-formatted for quick page display
	http.HandleFunc("/api/search-formatted", func(w http.ResponseWriter, r *http.Request) {
		apiSearchPageItem(store, w, r)
	})
	http.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		apiSearch(store, w, r)
	})

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		showStatusPage(store, *imageDir, w, r)
	})

	http.HandleFunc("/", stuffStoreRoot)

	log.Printf("Listening on :%d", *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))

	var block_forever chan bool
	<-block_forever
}
