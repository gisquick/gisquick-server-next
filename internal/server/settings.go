package server

import (
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gisquick/gisquick-server/internal/domain"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

const MB int64 = 1024 * 1024

var MaxJSONSize int64 = 1 * MB

func (s *Server) handleGetProjectFiles() func(echo.Context) error {
	return func(c echo.Context) error {
		projectName := c.Get("project").(string)
		files, err := s.projects.ListProjectFiles(projectName, true)
		if err != nil {
			if errors.Is(err, domain.ErrProjectNotExists) {
				return echo.NewHTTPError(http.StatusBadRequest, "Project does not exists")
			}
			return fmt.Errorf("handleGetProjectFiles: %w", err)
		}
		return c.JSON(http.StatusOK, files)
	}
}

func (s *Server) handleGetProjects(c echo.Context) error {
	user, err := s.auth.GetUser(c)
	if err != nil {
		return err
	}
	data, err := s.projects.GetUserProjects(user.Username)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, data)
}

func (s *Server) handleDeleteProject(c echo.Context) error {
	projectName := c.Get("project").(string)
	if err := s.projects.Delete(projectName); err != nil {
		if errors.Is(err, domain.ErrProjectNotExists) {
			return echo.NewHTTPError(http.StatusBadRequest, "Project does not exists")
		}
		return err
	}
	return c.NoContent(http.StatusOK)
}

// ProgressReader export
type ProgressReader struct {
	Reader   io.ReadCloser
	Callback func(int)
	Step     int
	Progress int
	lastVal  int
}

func (r *ProgressReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.Progress += n
	if r.Progress-r.lastVal >= r.Step || err == io.EOF {
		r.Callback(r.Progress)
		r.lastVal = r.Progress
	}
	return
}

func (r *ProgressReader) Close() error {
	return r.Reader.Close()
}

func (s *Server) handleUpload() func(echo.Context) error {
	type fileUploadProgress struct {
		File     string `json:"file"`
		Progress int    `json:"progress"`
	}
	type uploadInfo struct {
		Files []domain.ProjectFile `json:"files"`
	}

	return func(c echo.Context) error {
		req := c.Request()
		ctype, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if err != nil || ctype != "multipart/form-data" {
			return echo.NewHTTPError(http.StatusBadRequest, http.ErrNotMultipart.Error())
		}
		boundary, ok := params["boundary"]
		if !ok {
			return echo.NewHTTPError(http.StatusBadRequest, http.ErrMissingBoundary.Error())
		}
		user, err := s.auth.GetUser(c)
		if err != nil {
			return err
		}
		if !user.IsSuperuser {
			MaxProjectSize := int64(100 * 1024 * 1024) // TODO
			req.Body = http.MaxBytesReader(c.Response(), req.Body, MaxProjectSize)
		}
		reader := multipart.NewReader(req.Body, boundary)
		projectName := c.Get("project").(string)

		// first part should contain upload info
		var info uploadInfo
		part, err := reader.NextPart()
		if err != nil {
			s.log.Errorw("uploading files", "project", projectName, zap.Error(err))
			return err
		}
		err = json.NewDecoder(part).Decode(&info)
		if err != nil {
			s.log.Errorw("decoding upload metadata", zap.Error(err))
			return err
		}
		s.log.Infow("project files upload", "project", projectName)
		s.log.Infow("project files upload", "payload", info)

		// Ver. 1
		uploadProgress := make(map[string]int)
		lastNotification := time.Now()
		nextFile := func() (string, io.ReadCloser, error) { // or ReadCloser?
			part, err := reader.NextPart()
			if err != nil {
				if err == io.EOF {
					s.sws.AppChannel().Send(user.Username, "UploadProgress", uploadProgress)
				}
				return "", nil, err
			}
			var partReader io.ReadCloser = part
			if strings.HasSuffix(part.FileName(), ".gz") && !strings.HasSuffix(part.FormName(), ".gz") {
				partReader, _ = gzip.NewReader(part)
			}
			pr := &ProgressReader{Reader: partReader, Step: 32 * 1024, Callback: func(p int) {
				uploadProgress[part.FormName()] = p
				now := time.Now()
				if now.Sub(lastNotification).Seconds() > 0.5 {
					// if appWs := s.appsWs.Get(username); appWs != nil {
					// 	s.sendJSONMessage(appWs, "UploadProgress", uploadProgress)
					// }
					s.sws.AppChannel().Send(user.Username, "UploadProgress", uploadProgress)
					lastNotification = now
					uploadProgress = make(map[string]int)
				}
			}}
			return part.FormName(), pr, nil
		}
		changes := domain.FilesChanges{Updates: info.Files}
		if _, err := s.projects.UpdateFiles(projectName, changes, nextFile); err != nil {
			return err
		}
		// Ver. 2
		/*
			uploadProgress := make(map[string]int)
			lastNotification := time.Now()
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					s.sws.AppChannel().Send(user.Username, "UploadProgress", uploadProgress)
					// if appWs :=  appsWs.Get(username); appWs != nil {
					// 	s.sendJSONMessage(appWs, "UploadProgress", uploadProgress)
					// }
					break
				}
				var partReader io.ReadCloser = part
				if strings.HasSuffix(part.FileName(), ".gz") && !strings.HasSuffix(part.FormName(), ".gz") {
					partReader, _ = gzip.NewReader(part)
				}
				pr := &ProgressReader{Reader: partReader, Step: 32 * 1024, Callback: func(p int) {
					uploadProgress[part.FormName()] = p
					now := time.Now()
					if now.Sub(lastNotification).Seconds() > 0.5 {
						// if appWs := s.appsWs.Get(username); appWs != nil {
						// 	s.sendJSONMessage(appWs, "UploadProgress", uploadProgress)
						// }
						s.sws.AppChannel().Send(user.Username, "UploadProgress", uploadProgress)
						lastNotification = now
						uploadProgress = make(map[string]int)
					}
				}}
				s.projects.SaveFile(projectName, part.FormName(), pr)
				partReader.Close()
				if err != nil {
					return err
				}
			}
		*/
		return c.NoContent(http.StatusOK)
	}
}

func (s *Server) handleDeleteProjectFiles() func(echo.Context) error {
	type FilesInfo struct {
		Files []string `json:"files"`
	}

	return func(c echo.Context) error {
		projectName := c.Get("project").(string)
		var data FilesInfo
		if err := (&echo.DefaultBinder{}).BindBody(c, &data); err != nil {
			return err
		}
		if len(data.Files) < 1 {
			return echo.NewHTTPError(http.StatusBadRequest, "No files specified")
		}
		changes := domain.FilesChanges{Removes: data.Files}
		// nextFile := func() (string, io.ReadCloser, error) {
		// 	return "", nil, io.EOF
		// }
		files, err := s.projects.UpdateFiles(projectName, changes, nil)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, files)
	}
}

func (s *Server) handleGetMap() func(echo.Context) error {
	type RequestParams struct {
		Map string `query:"map"`
	}
	director := func(req *http.Request) {
		target, _ := url.Parse(s.Config.MapserverURL)
		// query := req.URL.Query()
		// project := req.URL.Query().Get("MAP")
		// req.URL.RawQuery = query.Encode()
		s.log.Infow("Map proxy", "query", req.URL.RawQuery)
		req.URL.Path = target.Path
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host

		if _, ok := req.Header["User-Agent"]; !ok {
			// explicitly disable User-Agent so it's not set to default value
			req.Header.Set("User-Agent", "")
		}
	}
	reverseProxy := &httputil.ReverseProxy{Director: director}
	reverseProxy.ErrorHandler = func(rw http.ResponseWriter, r *http.Request, e error) {
		s.log.Errorw("mapserver proxy error", zap.Error(e))
	}
	// reverseProxy.ErrorLog.SetOutput(os.Stdout)
	return func(c echo.Context) error {
		// params := new(RequestParams)
		// if err := (&echo.DefaultBinder{}).BindQueryParams(c, params); err != nil {
		// 	return echo.NewHTTPError(http.StatusBadRequest, "Invalid query parameters")
		// }

		// project := params.Map
		// user, err := s.auth.GetUser(c)
		// if err != nil {
		// 	return err
		// }
		// c.Request().URL.Query()
		projectName := c.Get("project").(string)

		p, err := s.projects.GetProjectInfo(projectName)
		if err != nil {
			if errors.Is(err, domain.ErrProjectNotExists) {
				return echo.NewHTTPError(http.StatusBadRequest, "Project does not exists")
			}
			return err
		}
		// TODO: hardcoded /publish/ directory!
		owsProject := filepath.Join("/publish/", projectName, p.QgisFile)
		s.log.Infow("GetMap", "ows_project", owsProject)
		query := c.Request().URL.Query()
		query.Set("MAP", owsProject)
		c.Request().URL.RawQuery = query.Encode()

		reverseProxy.ServeHTTP(c.Response(), c.Request())
		return nil
	}
}

func (s *Server) handleCreateProject() func(echo.Context) error {
	type Info struct {
		File        string            `json:"file"`
		ProjectHash string            `json:"project_hash"`
		Projection  domain.Projection `json:"projection"`
	}
	return func(c echo.Context) error {
		// TODO: check project folder/index file doesn't exists
		req := c.Request()
		req.Body = http.MaxBytesReader(c.Response(), req.Body, MaxJSONSize)
		defer req.Body.Close()

		var data json.RawMessage
		d := json.NewDecoder(req.Body)
		if err := d.Decode(&data); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request data")
		}
		username := c.Param("user")
		name := c.Param("name")

		info, err := s.projects.Create(username, name, data)
		if err != nil {
			if errors.Is(err, domain.ErrProjectAlreadyExists) {
				return echo.NewHTTPError(http.StatusConflict, "Project already exists")
			}
			return err
		}
		s.log.Infow("Created project", "info", info)
		return c.JSON(http.StatusOK, info)
	}
}

func (s *Server) handleGetProjectFullInfo() func(echo.Context) error {
	type FullInfo struct {
		Name       string          `json:"name"`
		Title      string          `json:"title"`
		Created    time.Time       `json:"created"`
		LastUpdate time.Time       `json:"last_update"`
		State      string          `json:"state"`
		Size       int             `json:"size"`
		Thumbnail  bool            `json:"thumbnail"`
		Meta       domain.QgisMeta `json:"meta"`
		// Meta     json.RawMessage         `json:"meta"`
		Settings *domain.ProjectSettings `json:"settings"`
		Scripts  domain.Scripts          `json:"scripts"`
	}
	return func(c echo.Context) error {
		projectName := c.Get("project").(string)
		info, err := s.projects.GetProjectInfo(projectName)
		if err != nil {
			if errors.Is(err, domain.ErrProjectNotExists) {
				return echo.NewHTTPError(http.StatusBadRequest, "Project does not exists")
			}
			return fmt.Errorf("[handleGetProjectInfo] loading project info: %w", err)
		}
		// var meta json.RawMessage
		var meta domain.QgisMeta
		if err := s.projects.GetQgisMetadata(projectName, &meta); err != nil {
			return fmt.Errorf("[handleGetProjectInfo] loading qgis meta: %w", err)
		}
		// meta, err := s.projects.GetQgisMetadata(projectName)
		// if err != nil {
		// 	return fmt.Errorf("[handleGetProjectInfo] loading qgis meta: %w", err)
		// }
		data := FullInfo{
			Name:       projectName,
			Title:      info.Title,
			Created:    info.Created,
			LastUpdate: info.LastUpdate,
			State:      info.State,
			Size:       info.Size,
			Thumbnail:  info.Thumbnail,
			Meta:       meta,
		}
		if info.State != "empty" {
			settings, err := s.projects.GetSettings(projectName)
			if err == nil {
				data.Settings = &settings
			} else {
				s.log.Warnw("[handleGetProjectInfo] settings not found", "project", projectName, zap.Error(err))
			}
		}
		scripts, err := s.projects.GetScripts(projectName)
		if err != nil {
			s.log.Errorw("[handleGetProjectInfo] loading scripts", "project", projectName)
		} else {
			data.Scripts = scripts
		}
		return c.JSON(http.StatusOK, data)
	}
}

func (s *Server) handleGetProjectInfo(c echo.Context) error {
	projectName := c.Get("project").(string)
	info, err := s.projects.GetProjectInfo(projectName)
	if err != nil {
		if errors.Is(err, domain.ErrProjectNotExists) {
			return echo.NewHTTPError(http.StatusBadRequest, "Project does not exists")
		}
		return fmt.Errorf("handleGetProjectInfo: %w", err)
	}
	return c.JSON(http.StatusOK, info)
}

func (s *Server) handleUpdateProjectMeta() func(echo.Context) error {
	type Info struct {
		File        string            `json:"file"`
		ProjectHash string            `json:"project_hash"`
		Projection  domain.Projection `json:"projection"`
	}
	return func(c echo.Context) error {
		projectName := c.Get("project").(string)
		req := c.Request()
		req.Body = http.MaxBytesReader(c.Response(), req.Body, MaxJSONSize)
		defer req.Body.Close()

		var data json.RawMessage
		d := json.NewDecoder(req.Body)
		if err := d.Decode(&data); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request data")
		}

		err := s.projects.UpdateMeta(projectName, data)
		if err != nil {
			if errors.Is(err, domain.ErrProjectNotExists) {
				return echo.NewHTTPError(http.StatusConflict, "Project does not exists")
			}
			return err
		}
		return c.NoContent(http.StatusOK)
	}
}

/* Settings Handlers */

func (s *Server) handleSaveProjectSettings(c echo.Context) error {
	projectName := c.Get("project").(string)
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, MaxJSONSize)
	defer req.Body.Close()

	var data json.RawMessage
	d := json.NewDecoder(req.Body)
	if err := d.Decode(&data); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid request data")
	}
	return s.projects.UpdateSettings(projectName, data)
}

func (s *Server) handleUploadThumbnail(c echo.Context) error {
	if err := c.Request().ParseForm(); err != nil {
		return err
	}
	f, h, err := c.Request().FormFile("image")
	if err != nil {
		return err
	}
	defer f.Close()
	projectName := c.Get("project").(string)
	s.log.Infow("thumbnail", "project", projectName, "image", h.Filename)
	if err := s.projects.SaveThumbnail(projectName, f); err != nil {
		return err
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) handleGetThumbnail(c echo.Context) error {
	username := c.Param("user")
	name := c.Param("name")
	projectName := filepath.Join(username, name)
	return c.File(s.projects.GetThumbnailPath(projectName))
}

func (s *Server) handleProjectStaticFile(c echo.Context) error {
	username := c.Param("user")
	name := c.Param("name")
	projectName := filepath.Join(username, name)
	scriptPath := c.Param("*")
	root := s.projects.GetScriptsPath(projectName)
	return c.File(filepath.Join(root, scriptPath))
}

func (s *Server) handleScriptUpload() func(echo.Context) error {
	type Data struct {
		domain.ScriptModule
		Module string `json:"module"`
	}
	return func(c echo.Context) error {
		projectName := c.Get("project").(string)

		if err := c.Request().ParseMultipartForm(2 * MB); err != nil {
			return err
		}
		var info Data
		jsonInfo := c.FormValue("info")
		if err := json.Unmarshal([]byte(jsonInfo), &info); err != nil {
			s.log.Errorw("[handleScriptUpload] parsing metadata", zap.Error(err))
			return echo.ErrBadRequest
		}
		if info.Module == "" || len(info.Components) < 1 { // TODO: better name validation with regex
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid script module data")
		}
		var filesMeta []domain.ProjectFile = []domain.ProjectFile{}
		var files []*multipart.FileHeader = []*multipart.FileHeader{}

		form, err := c.MultipartForm()
		if err != nil {
			return err
		}
		for n, f := range form.File {
			s.log.Infow("[handleScriptUpload]", "file", n, "len", len(f))
			for _, fh := range f {
				path := filepath.Join("web", "components", fh.Filename)
				filesMeta = append(filesMeta, domain.ProjectFile{Path: path, Size: fh.Size})
				files = append(files, fh)
			}
		}
		changes := domain.FilesChanges{Updates: filesMeta}
		s.log.Infow("[handleScriptUpload]", "info", info, "changes", changes)

		findex := 0
		nextFile := func() (string, io.ReadCloser, error) { // or ReadCloser?
			if findex >= len(files) || findex >= len(filesMeta) {
				return "", nil, io.EOF
			}
			f := files[findex]
			path := filesMeta[findex].Path
			file, err := f.Open()
			if err != nil {
				return "", nil, err
			}
			findex += 1
			return path, file, nil
		}
		if _, err := s.projects.UpdateFiles(projectName, changes, nextFile); err != nil {
			return fmt.Errorf("[handleScriptUpload] saving script file: %w", err)
		}
		scripts, err := s.projects.GetScripts(projectName)
		if err != nil {
			return fmt.Errorf("[handleScriptUpload] getting scripts metadata: %w", err)
		}
		s.log.Infow("handleScriptUpload", "scripts", scripts)
		if scripts == nil {
			scripts = make(domain.Scripts, 1)
		}
		info.Path = filepath.Join("components", info.Path)
		scripts[info.Module] = info.ScriptModule
		// s.log.Infow("[handleScriptUpload]", "scripts", scripts)

		if err := s.projects.UpdateScripts(projectName, scripts); err != nil {
			return fmt.Errorf("[handleScriptUpload] updating scripts metadata: %w", err)
		}
		return c.JSON(http.StatusOK, scripts)
	}
}

func (s *Server) handleDeleteScript() func(echo.Context) error {
	type Params struct {
		Modules []string `json:"modules"`
	}
	return func(c echo.Context) error {
		projectName := c.Get("project").(string)
		// var params Params
		var modules []string
		if err := (&echo.DefaultBinder{}).BindBody(c, &modules); err != nil {
			return err
		}
		scripts, err := s.projects.RemoveScripts(projectName, modules...)
		if err != nil {
			return fmt.Errorf("[handleDeleteScript] removing scripts: %w", err)
		}
		return c.JSON(http.StatusOK, scripts)
	}
}

func (s *Server) handleProjectFile(c echo.Context) error {
	projectName := c.Get("project").(string)
	filePath := c.Param("*")
	return c.File(filepath.Join(s.Config.ProjectsRoot, projectName, filePath))
}

func CopyFile(dest io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(dest, file)
	return err
}

func (s *Server) handleDownloadProjectFile(c echo.Context) error {
	projectName := c.Get("project").(string)
	filePath := c.Param("*")
	fullPath := filepath.Join(s.Config.ProjectsRoot, projectName, filePath)

	name := filepath.Base(filePath)

	info, err := os.Lstat(fullPath)
	if err != nil {
		return fmt.Errorf("getting file info: %w", err)
	}
	if info.IsDir() {
		c.Response().Header().Set("Content-Type", "application/octet-stream")
		c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.zip", name))
		writer := zip.NewWriter(c.Response())
		defer writer.Close()
		rootPath := filepath.Dir(fullPath)
		err := filepath.Walk(fullPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				// relPath2 := path[len(rootPath)+1:]
				relPath, _ := filepath.Rel(rootPath, path)
				s.log.Infow("download file", "rel", relPath)
				part, err := writer.Create(relPath)
				if err != nil {
					return err
				}
				return CopyFile(part, path)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("downloading project directory: %w", err)
		}
		return nil
	}
	return c.Attachment(fullPath, name)
}

func (s *Server) handleInlineProjectFile(c echo.Context) error {
	projectName := c.Get("project").(string)
	filePath := c.Param("*")
	name := filepath.Base(filePath)
	return c.Inline(filepath.Join(s.Config.ProjectsRoot, projectName, filePath), name)
}

func (s *Server) handleProjectReload(c echo.Context) error {
	client := &http.Client{}
	projectName := c.Get("project").(string)
	p, err := s.projects.GetProjectInfo(projectName)
	if err != nil {
		if errors.Is(err, domain.ErrProjectNotExists) {
			return echo.NewHTTPError(http.StatusBadRequest, "Project does not exists")
		}
		return err
	}
	// TODO: hardcoded /publish/ directory!
	owsProject := filepath.Join("/publish/", projectName, p.QgisFile)
	params := url.Values{"project": {owsProject}}

	req, err := http.NewRequest(http.MethodPost, s.Config.MapserverURL, nil)
	if err != nil {
		return fmt.Errorf("[handleProjectReload] building request: %w", err)
	}
	req.URL.Path = filepath.Join(req.URL.Path, "/reload")
	req.URL.RawQuery = params.Encode()
	// s.log.Infow("[handleProjectReload]", "project", projectName, "url", req.URL.String())

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mapserver request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := ioutil.ReadAll(resp.Body)
		s.log.Errorw("[handleProjectReload]", "project", projectName, "status", resp.StatusCode, "msg", string(msg))
		return fmt.Errorf("reloading project on qgis server: %s", string(msg))
	}
	return c.NoContent(http.StatusOK)
}