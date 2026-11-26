package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"
)

const dlnaUDN = "uuid:fast-fotos-1"

type dlnaContainer struct {
	ID     string
	Title  string
	Parent string
}

type dlnaDidl struct {
	XMLName xml.Name `xml:"DIDL-Lite"`
	XMLns   string   `xml:"xmlns,attr"`
	Dc      string   `xml:"xmlns:dc,attr,omitempty"`
	Upnp    string   `xml:"xmlns:upnp,attr,omitempty"`
	Items   []string `xml:"container|item"`
}

// startDLNADiscovery starts the server's background SSDP advertiser once.
// Input: the server; output: none.
func (s *Server) startDLNADiscovery() {
	if s.dlnaCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.dlnaCancel = cancel
	go s.runDLNAAdvertiser(ctx)
}

// dlnaAddress determines the LAN address and configured HTTP port advertised to DLNA clients.
// Input: the server configuration; output: a host-and-port string.
func (s *Server) dlnaAddress() string {
	host := dlnaLocalAddress()
	port := "8090"
	if _, portText, err := net.SplitHostPort(s.config.ListenAddress); err == nil && portText != "" {
		port = portText
	}
	return net.JoinHostPort(host, port)
}

// dlnaLocalAddress returns the first active non-loopback IPv4 address, falling back to localhost.
// Input: none; output: an IPv4 address string.
func dlnaLocalAddress() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil {
				return ip.String()
			}
		}
	}
	return "127.0.0.1"
}

// dlnaDescription serves the UPnP device description XML for this media server.
// Inputs: an HTTP response writer and request; output: a device-description XML response.
func (s *Server) dlnaDescription(w http.ResponseWriter, r *http.Request) {
	address := s.dlnaAddress()
	body := fmt.Sprintf(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>Fast Fotos</friendlyName>
    <manufacturer>Fast Fotos</manufacturer>
    <manufacturerURL>https://github.com/andrewl/fast-fotos</manufacturerURL>
    <modelName>Fast Fotos Media Server</modelName>
    <modelDescription>Fast Fotos DLNA photo server</modelDescription>
    <modelNumber>1</modelNumber>
    <UDN>%s</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId>
        <SCPDURL>/dlna/contentdirectory.xml</SCPDURL>
        <controlURL>/dlna/control</controlURL>
        <eventSubURL>/dlna/event</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>`, dlnaUDN)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = io.WriteString(w, body)
	_ = address
}

// dlnaContentDirectoryDescription serves the UPnP ContentDirectory service description XML.
// Inputs: an HTTP response writer and request; output: a service-description XML response.
func (s *Server) dlnaContentDirectoryDescription(w http.ResponseWriter, r *http.Request) {
	body := `<?xml version="1.0"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <actionList>
    <action><name>Browse</name>
      <argumentList>
        <argument><name>ObjectID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable></argument>
        <argument><name>BrowseFlag</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_BrowseFlag</relatedStateVariable></argument>
        <argument><name>Filter</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable></argument>
        <argument><name>StartingIndex</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable></argument>
        <argument><name>RequestedCount</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>SortCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable></argument>
        <argument><name>Result</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable></argument>
        <argument><name>NumberReturned</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>TotalMatches</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>UpdateID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable></argument>
      </argumentList>
    </action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ObjectID</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_BrowseFlag</name><dataType>string</dataType><allowedValueList><allowedValue>BrowseMetadata</allowedValue><allowedValue>BrowseDirectChildren</allowedValue></allowedValueList></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Filter</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Index</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Count</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_SortCriteria</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Result</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_UpdateID</name><dataType>ui4</dataType></stateVariable>
  </serviceStateTable>
</scpd>`
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

// runDLNAAdvertiser listens for SSDP discovery requests until ctx is cancelled.
// Inputs: the server and lifecycle context; output: none.
func (s *Server) runDLNAAdvertiser(ctx context.Context) {
	conn, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900})
	if err != nil {
		slog.Debug("DLNA SSDP listener unavailable", "error", err)
		return
	}
	defer conn.Close()
	go s.runDLNAAnnouncements(ctx, conn)
	buffer := make([]byte, 2048)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, remote, err := conn.ReadFromUDP(buffer)
		if err != nil {
			continue
		}
		request := string(buffer[:n])
		if strings.HasPrefix(request, "M-SEARCH ") {
			s.sendDLNAMSearchResponse(conn, remote)
		}
	}
}

// runDLNAAnnouncements broadcasts periodic SSDP alive and shutdown announcements.
// Inputs: the lifecycle context and UDP connection; output: none.
func (s *Server) runDLNAAnnouncements(ctx context.Context, conn *net.UDPConn) {
	s.sendDLNAAnnouncements(conn, "ssdp:alive")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.sendDLNAAnnouncements(conn, "ssdp:byebye")
			return
		case <-ticker.C:
			s.sendDLNAAnnouncements(conn, "ssdp:alive")
		}
	}
}

// sendDLNAMSearchResponse sends the media-server response to an SSDP M-SEARCH requester.
// Inputs: a UDP connection and remote address; output: none.
func (s *Server) sendDLNAMSearchResponse(conn *net.UDPConn, remote *net.UDPAddr) {
	address := s.dlnaAddress()
	location := "http://" + address + "/dlna/description.xml"
	response := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nLOCATION: %s\r\nST: urn:schemas-upnp-org:device:MediaServer:1\r\nUSN: %s::urn:schemas-upnp-org:device:MediaServer:1\r\nSERVER: Fast-Fotos/1 UPnP/1.0\r\n\r\n", location, dlnaUDN)
	_, _ = conn.WriteToUDP([]byte(response), remote)
}

// sendDLNAAnnouncements broadcasts the given SSDP notification type for supported device services.
// Inputs: a UDP connection and notification subtype; output: none.
func (s *Server) sendDLNAAnnouncements(conn *net.UDPConn, nts string) {
	address := s.dlnaAddress()
	location := "http://" + address + "/dlna/description.xml"
	target := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	for _, nt := range []string{"upnp:rootdevice", "urn:schemas-upnp-org:device:MediaServer:1", "urn:schemas-upnp-org:service:ContentDirectory:1"} {
		usn := dlnaUDN
		if nt != "upnp:rootdevice" {
			usn += "::" + nt
		}
		body := fmt.Sprintf("NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nCACHE-CONTROL: max-age=1800\r\nLOCATION: %s\r\nNT: %s\r\nNTS: %s\r\nUSN: %s\r\nSERVER: Fast-Fotos/1 UPnP/1.0\r\n\r\n", location, nt, nts, usn)
		_, _ = conn.WriteToUDP([]byte(body), target)
	}
}

type dlnaBrowseRequest struct {
	ObjectID       string `xml:"ObjectID"`
	BrowseFlag     string `xml:"BrowseFlag"`
	Filter         string `xml:"Filter"`
	StartingIndex  int    `xml:"StartingIndex"`
	RequestedCount int    `xml:"RequestedCount"`
}

// dlnaControl parses a SOAP Browse request and returns matching ContentDirectory results.
// Inputs: an HTTP response writer and SOAP request; output: a SOAP XML response or HTTP error.
func (s *Server) dlnaControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Could not read SOAP request", http.StatusBadRequest)
		return
	}
	var envelope struct {
		XMLName xml.Name
		Body    struct {
			Browse struct {
				ObjectID       string `xml:"ObjectID"`
				BrowseFlag     string `xml:"BrowseFlag"`
				Filter         string `xml:"Filter"`
				StartingIndex  int    `xml:"StartingIndex"`
				RequestedCount int    `xml:"RequestedCount"`
			} `xml:"Browse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "Invalid SOAP request", http.StatusBadRequest)
		return
	}
	request := envelope.Body.Browse
	didl, total, err := s.dlnaBrowse(r.Context(), request.ObjectID, request.BrowseFlag, request.StartingIndex, request.RequestedCount)
	if err != nil {
		slog.Error("DLNA browse failed", "object_id", request.ObjectID, "error", err)
		http.Error(w, "Could not browse media", http.StatusInternalServerError)
		return
	}
	response := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
  <s:Body>
    <u:BrowseResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">
      <Result>%s</Result>
      <NumberReturned>%d</NumberReturned>
      <TotalMatches>%d</TotalMatches>
      <UpdateID>1</UpdateID>
    </u:BrowseResponse>
  </s:Body>
</s:Envelope>`, xmlEscape(didl), dlnaDIDLCount(didl), total)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = io.WriteString(w, response)
}

// xmlEscape escapes text for safe inclusion in XML.
// Input: unescaped text; output: its XML-escaped form.
func xmlEscape(value string) string {
	var escaped strings.Builder
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

// dlnaDIDLCount counts container and item elements in a DIDL-Lite document.
// Input: DIDL-Lite XML; output: the number of matched elements.
func dlnaDIDLCount(didl string) int {
	return strings.Count(didl, "<container ") + strings.Count(didl, "<item ")
}

// dlnaBrowse returns DIDL-Lite metadata or paginated children for a ContentDirectory object.
// Inputs: context, object ID, browse flag, start offset, and item limit; outputs: DIDL XML, total matches, or an error.
func (s *Server) dlnaBrowse(ctx context.Context, objectID, browseFlag string, start, count int) (string, int, error) {
	if strings.EqualFold(browseFlag, "BrowseMetadata") {
		containers, err := s.dlnaContainers(ctx)
		for _, container := range containers {
			if container.ID == objectID {
				return dlnaContainerDIDL(container), 1, err
			}
		}
		if strings.HasPrefix(objectID, "item/") {
			idText := strings.TrimPrefix(objectID, "item/")
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil {
				return "", 0, err
			}
			photo, err := s.db.PhotoByID(ctx, id)
			if err != nil {
				return "", 0, err
			}
			return dlnaItemDIDL(s, photo, "0"), 1, nil
		}
		return "", 0, nil
	}

	containers, err := s.dlnaContainers(ctx)
	if err != nil {
		return "", 0, err
	}
	if objectID == "" || objectID == "0" {
		var didl strings.Builder
		didl.WriteString(`<?xml version="1.0" encoding="utf-8"?><DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
		for _, container := range containers {
			if container.Parent == "0" {
				didl.WriteString(dlnaContainerXML(container))
			}
		}
		didl.WriteString(`</DIDL-Lite>`)
		return didl.String(), dlnaDIDLCount(didl.String()), nil
	}

	for _, container := range containers {
		if container.Parent == objectID {
			var didl strings.Builder
			didl.WriteString(`<?xml version="1.0" encoding="utf-8"?><DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
			for _, child := range containers {
				if child.Parent == objectID {
					didl.WriteString(dlnaContainerXML(child))
				}
			}
			didl.WriteString(`</DIDL-Lite>`)
			return didl.String(), dlnaDIDLCount(didl.String()), nil
		}
	}

	photos, err := s.dlnaPhotosForContainer(ctx, objectID)
	if err != nil {
		return "", 0, err
	}
	total := len(photos)
	if start > 0 {
		if start >= len(photos) {
			photos = nil
		} else {
			photos = photos[start:]
		}
	}
	if count > 0 && count < len(photos) {
		photos = photos[:count]
	}
	var didl strings.Builder
	didl.WriteString(`<?xml version="1.0" encoding="utf-8"?><DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
	for _, photo := range photos {
		didl.WriteString(dlnaItemDIDL(s, photo, objectID))
	}
	didl.WriteString(`</DIDL-Lite>`)
	return didl.String(), total, nil
}

// dlnaContainers builds the DLNA hierarchy from timeline months, saved searches, and galleries.
// Input: a database context; outputs: available containers or an error.
func (s *Server) dlnaContainers(ctx context.Context) ([]dlnaContainer, error) {
	containers := []dlnaContainer{
		{ID: "timeline", Title: "Timeline", Parent: "0"},
		{ID: "this-week", Title: "This Week", Parent: "0"},
		{ID: "searches", Title: "Saved Searches", Parent: "0"},
		{ID: "galleries", Title: "Galleries", Parent: "0"},
	}

	months, err := s.db.Months(ctx)
	if err != nil {
		return nil, err
	}
	for _, month := range months {
		containers = append(containers, dlnaContainer{
			ID: "timeline/" + month.Key, Title: monthLabel(month.Key), Parent: "timeline",
		})
	}

	searches, err := s.db.SavedSearches(ctx)
	if err == nil {
		for _, search := range searches {
			containers = append(containers, dlnaContainer{
				ID: "search/" + strconv.FormatInt(search.ID, 10), Title: search.Title, Parent: "searches",
			})
		}
	}

	galleries, err := s.db.Collections(ctx)
	if err != nil {
		return nil, err
	}
	for _, gallery := range galleries {
		if gallery.Name == SelectedCollectionName {
			continue
		}
		containers = append(containers, dlnaContainer{
			ID: "gallery/" + strconv.FormatInt(gallery.ID, 10), Title: gallery.Name, Parent: "galleries",
		})
	}
	return containers, nil
}

// dlnaPhotosForContainer resolves a DLNA leaf object ID to its photo records.
// Inputs: a database context and object ID; outputs: matching photos or an error.
func (s *Server) dlnaPhotosForContainer(ctx context.Context, objectID string) ([]Photo, error) {
	if objectID == "this-week" {
		return s.photosForThisWeek(ctx)
	}
	if strings.HasPrefix(objectID, "timeline/") {
		month := strings.TrimPrefix(objectID, "timeline/")
		return s.db.PhotosForMonth(ctx, month)
	}
	if strings.HasPrefix(objectID, "gallery/") {
		id, err := strconv.ParseInt(strings.TrimPrefix(objectID, "gallery/"), 10, 64)
		if err != nil {
			return nil, err
		}
		return s.db.PhotosForCollection(ctx, id)
	}
	if strings.HasPrefix(objectID, "search/") {
		id, err := strconv.ParseInt(strings.TrimPrefix(objectID, "search/"), 10, 64)
		if err != nil {
			return nil, err
		}
		searches, err := s.db.SavedSearches(ctx)
		if err != nil {
			return nil, err
		}
		for _, search := range searches {
			if search.ID == id {
				criteria := strings.TrimPrefix(search.URL, "/search/")
				query, err := parseSearchCollectionCriteria(criteria)
				if err != nil {
					return nil, err
				}
				searchData := SearchData{
					DateFrom: query.Get("from"), DateTo: query.Get("to"), Label: query.Get("label"),
					CameraModel: query.Get("camera"), FocalLength: query.Get("focal"),
					Flash: query.Get("flash"), Location: query.Get("location"), Color: query.Get("color"),
				}
				if strings.HasPrefix(searchData.Location, "settlement:") {
					searchData.Settlement = strings.TrimPrefix(searchData.Location, "settlement:")
				} else if strings.HasPrefix(searchData.Location, "region:") {
					searchData.Region = strings.TrimPrefix(searchData.Location, "region:")
				}
				return s.db.SearchPhotos(ctx, searchData)
			}
		}
		return nil, sql.ErrNoRows
	}
	return nil, nil
}

// dlnaContainerXML serializes one DLNA container as an XML element.
// Input: a container record; output: its escaped XML representation.
func dlnaContainerXML(container dlnaContainer) string {
	return fmt.Sprintf(`<container id="%s" parentID="%s" restricted="1" searchable="0"><dc:title>%s</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`,
		xmlEscape(container.ID), xmlEscape(container.Parent), xmlEscape(container.Title))
}

// dlnaContainerDIDL wraps one serialized container in a DIDL-Lite document.
// Input: a container record; output: a complete DIDL-Lite XML document.
func dlnaContainerDIDL(container dlnaContainer) string {
	return `<?xml version="1.0" encoding="utf-8"?><DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` +
		dlnaContainerXML(container) + `</DIDL-Lite>`
}

// dlnaItemDIDL serializes a photo and its resource URLs as a DIDL-Lite item.
// Inputs: the server, photo record, and parent container ID; output: the item XML.
func dlnaItemDIDL(s *Server, photo Photo, parentID string) string {
	address := s.dlnaAddress()
	title := filepath.Base(photo.Path)
	if title == "" {
		title = "Photo " + strconv.FormatInt(photo.ID, 10)
	}
	resourceURL := "http://" + address + "/dlna/resource/" + strconv.FormatInt(photo.ID, 10)
	thumbnailURL := "http://" + address + "/dlna/thumbnail/" + strconv.FormatInt(photo.ID, 10)
	mime := "image/jpeg"
	if photo.IsVideo() {
		mime = "video/mp4"
	} else {
		extension := strings.ToLower(filepath.Ext(photo.Path))
		switch extension {
		case ".png":
			mime = "image/png"
		case ".gif":
			mime = "image/gif"
		case ".webp":
			mime = "image/webp"
		case ".tif", ".tiff":
			mime = "image/tiff"
		}
	}
	return fmt.Sprintf(`<item id="item/%d" parentID="%s" restricted="1"><dc:title>%s</dc:title><upnp:class>object.item.imageItem.photo</upnp:class><upnp:albumArtURI>%s</upnp:albumArtURI><res protocolInfo="http-get:*:%s:*" thumbnailInfo="http-get:*:image/jpeg:*">%s</res></item>`,
		photo.ID, xmlEscape(parentID), xmlEscape(title), xmlEscape(thumbnailURL), mime, xmlEscape(resourceURL))
}

// dlnaThumbnail serves a stored photo thumbnail to a DLNA client.
// Inputs: an HTTP response writer and request containing the photo ID; output: JPEG data or HTTP not-found.
func (s *Server) dlnaThumbnail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/dlna/thumbnail/")
	thumbnail, path, err := s.db.PhotoThumbnail(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if len(thumbnail) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_ = path
	_, _ = w.Write(thumbnail)
}

// scaleDLNAImage converts oversized supported images to JPEG within 4K bounds.
// Input: an image path; outputs: encoded bytes, MIME type, whether conversion occurred, or an error.
func scaleDLNAImage(path string) ([]byte, string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", false, err
	}
	defer file.Close()
	source, _, err := image.Decode(file)
	if err != nil {
		return nil, "", false, nil
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, "", false, fmt.Errorf("image has invalid dimensions %dx%d", width, height)
	}
	if width <= 3840 && height <= 2160 {
		return nil, "", false, nil
	}
	scale := 1.0
	if float64(width)/3840.0 > float64(height)/2160.0 {
		scale = 3840.0 / float64(width)
	} else {
		scale = 2160.0 / float64(height)
	}
	targetWidth := int(float64(width)*scale + 0.5)
	targetHeight := int(float64(height)*scale + 0.5)
	filter, err := resizeFilter("bilinear")
	if err != nil {
		return nil, "", false, err
	}
	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	filter.Scale(target, target.Bounds(), source, bounds, draw.Over, nil)
	var output bytes.Buffer
	if err := jpeg.Encode(&output, target, &jpeg.Options{Quality: 92}); err != nil {
		return nil, "", false, err
	}
	return output.Bytes(), "image/jpeg", true, nil
}

// dlnaResource safely serves an indexed source image or video, resizing oversized images for DLNA.
// Inputs: an HTTP response writer and request containing the photo ID; output: source media or an HTTP error.
func (s *Server) dlnaResource(w http.ResponseWriter, r *http.Request) {
	idText := strings.TrimPrefix(r.URL.Path, "/dlna/resource/")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	photo, err := s.db.PhotoByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(s.config.PhotoRoot, photo.Path)
	resolved, err := filepath.Abs(fullPath)
	if err != nil {
		http.Error(w, "Could not resolve photo", http.StatusInternalServerError)
		return
	}
	root, err := filepath.Abs(s.config.PhotoRoot)
	if err != nil || !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(resolved); err != nil {
		http.NotFound(w, r)
		return
	}
	extension := strings.ToLower(filepath.Ext(resolved))
	if extension != ".gif" && extension != ".mp4" && extension != ".m4v" &&
		extension != ".mov" && extension != ".mkv" {
		resized, resizedMime, changed, err := scaleDLNAImage(resolved)
		if err != nil {
			slog.Error("resize DLNA image", "path", resolved, "error", err)
		} else if changed {
			w.Header().Set("Content-Type", resizedMime)
			w.Header().Set("Cache-Control", "private, max-age=86400")
			w.Header().Set("Content-Length", strconv.Itoa(len(resized)))
			_, _ = w.Write(resized)
			return
		}
	}
	mime := "image/jpeg"
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	case ".tif", ".tiff":
		mime = "image/tiff"
	case ".mp4", ".m4v":
		mime = "video/mp4"
	case ".mov":
		mime = "video/quicktime"
	case ".mkv":
		mime = "video/x-matroska"
	}
	w.Header().Set("Content-Type", mime)
	http.ServeFile(w, r, resolved)
}
