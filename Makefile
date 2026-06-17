# Define the target name and Go source files
# $Id: Makefile,v 1.23 2026/02/17 13:51:14 ralph Exp $

# Variables
TITLE = "ROSE SWE Updater - A tool to update ROSE Online client files from the official download page"
INPUT = README.md
OUTPUT = readme.html
TEMPLATES = .tmp_header.html .tmp_footer.html

TARGET = Updater
GENERATOR = ManifestGenerator
GOFILES = .
GENERATOR_PKG = ./cmd/manifestgenerator
BUILD_DIR = build
VERSION=$(shell git describe --tags --abbrev=0 | sed 's/v//g')

# Define cross-compilation settings for Windows and Linux (32-bit)
WINDOWS_ARCH = windows/386
LINUX_ARCH = linux/386

# Global Go flags to remove BuildID, omits the DWARF symbol table, removes the symbol table and debug information.
GOFLAGS := -ldflags "-buildid= -w -s "
##  -buildmode=pie

# Define a variable to hold the date
current_date_mon := $(shell date +%Y%m)
current_date_full := $(shell date +%Y%m%d-%H:%M)

# Default target
all: build-windows build-linux built-darwin html
.PHONY: all html clean


# Create the build directory if it does not exist
$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

# MacOS, 64bit only, no strip working here
built-darwin:
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(TARGET)-darwin64  $(GOFILES)
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(GENERATOR)-darwin64  $(GENERATOR_PKG)


# Windows (32+64-bit) build
build-windows: $(BUILD_DIR)
	goversioninfo -icon main.ico -o resource.syso versioninfo.json
	GOOS=windows GOARCH=386 go build ${GOFLAGS} -o $(BUILD_DIR)/$(TARGET)32.exe $(GOFILES)
	strip $(BUILD_DIR)/$(TARGET)32.exe
	@echo "[!] Windows 32-bit executable created: $(BUILD_DIR)/$(TARGET)32.exe"
	GOOS=windows GOARCH=386 go build ${GOFLAGS} -o $(BUILD_DIR)/$(GENERATOR)32.exe $(GENERATOR_PKG)
	strip $(BUILD_DIR)/$(GENERATOR)32.exe
	@echo "[!] Windows 32-bit executable created: $(BUILD_DIR)/$(GENERATOR)32.exe"
	GOOS=windows GOARCH=amd64 go build ${GOFLAGS} -o $(BUILD_DIR)/$(TARGET)64.exe $(GOFILES)
	strip $(BUILD_DIR)/$(TARGET)64.exe
	@echo "[!] Windows 64-bit executable created: $(BUILD_DIR)/$(TARGET)64.exe"
	GOOS=windows GOARCH=amd64 go build ${GOFLAGS} -o $(BUILD_DIR)/$(GENERATOR)64.exe $(GENERATOR_PKG)
	strip $(BUILD_DIR)/$(GENERATOR)64.exe
	@echo "[!] Windows 64-bit executable created: $(BUILD_DIR)/$(GENERATOR)64.exe"

# Linux (32+64-bit) build
build-linux: $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=386 go build ${GOFLAGS} -o $(BUILD_DIR)/$(TARGET)_i686 $(GOFILES)
	strip $(BUILD_DIR)/$(TARGET)_i686
	@echo "[!] Linux 32-bit executable created: $(BUILD_DIR)/$(TARGET)_i686"
	CGO_ENABLED=0 GOOS=linux GOARCH=386 go build ${GOFLAGS} -o $(BUILD_DIR)/$(GENERATOR)_i686 $(GENERATOR_PKG)
	strip $(BUILD_DIR)/$(GENERATOR)_i686
	@echo "[!] Linux 32-bit executable created: $(BUILD_DIR)/$(GENERATOR)_i686"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${GOFLAGS} -o $(BUILD_DIR)/$(TARGET)_amd64 $(GOFILES)
	strip $(BUILD_DIR)/$(TARGET)_amd64
	@echo "[!] Linux 32-bit executable created: $(BUILD_DIR)/$(TARGET)_amd64"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${GOFLAGS} -o $(BUILD_DIR)/$(GENERATOR)_amd64 $(GENERATOR_PKG)
	strip $(BUILD_DIR)/$(GENERATOR)_amd64
	@echo "[!] Linux 64-bit executable created: $(BUILD_DIR)/$(GENERATOR)_amd64"

# Clean up the build directory, cleanup misc. stuff
clean:
	rm -rf $(BUILD_DIR)  rupdater2_*.zip  $(OUTPUT)	ChangeLog.txt files.txt
	go telemetry off
	go mod tidy
	@echo "[!] Build directory cleaned. Mod tidied."

# To force re-build
rebuild: clean all
	@echo "[!] Rebuilding project..."

dist: clean all html
	file $(BUILD_DIR)/$(TARGET)* > files.txt
	file $(BUILD_DIR)/$(GENERATOR)* >> files.txt
	echo "" >> files.txt
	echo "## GLIBC requirements" >> files.txt
	ldd -v $(BUILD_DIR)/$(TARGET)* >> files.txt || true
	ldd -v $(BUILD_DIR)/$(GENERATOR)* >> files.txt || true
	upx --best --lzma --force-macos $(BUILD_DIR)/$(TARGET)*
	upx --best --lzma --force-macos $(BUILD_DIR)/$(GENERATOR)*
	#pandoc README.md -t HTML -o readme.html
	zip -j "rupdater2_$(current_date_mon).zip"  readme.html $(BUILD_DIR)/$(TARGET)* $(BUILD_DIR)/$(GENERATOR)* ChangeLog.txt rupdater_example.txt files.txt
	describe "rupdater2_$(current_date_mon).zip" -desc="ROSE SWE updater release2 - downloads new software from the ROSE SWE download page"

changelog:
	gitchangelog > ChangeLog.txt
	git commit -a -s -m "chg: Updated the Changelog (by Makefile) $(current_date_full)"
	cat ChangeLog.txt

# Help message
help:
	@echo "|== Makefile commands =="
	@echo " 	 make build    			- Build the binary"
	@echo " 	 make dist     			- Build and make the ZIP for distribution"
	@echo " 	 make clean    			- Clean up build artifacts"
	@echo " 	 make rebuild  			- Clean and rebuild the project"
	@echo " 	 make changelog			- Generate a changelog from git commits"
	@echo " 	 make all      			- Build for all platforms"
	@echo " 	 make build-windows - Build for Windows"
	@echo " 	 make build-linux   - Build for Linux"
	@echo " 	 make build-darwin  - Build for MacOS"
	@echo " 	 make help          - Show this help message"
	@echo "    make html          - Generate polished HTML from README.md"

html:
	@echo "Style: Polishing $(INPUT) into $(OUTPUT)..."
	@# Create temporary header with CSS
	@echo '<style>\
	  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; line-height: 1.6; color: #24292e; max-width: 850px; margin: 40px auto; padding: 0 30px; background-color: #f6f8fa; }\
	  .main-card { background: white; padding: 45px; border: 1px solid #d1d5da; border-radius: 6px; box-shadow: 0 8px 24px rgba(149,157,165,0.1); }\
	  h1 { color: #0366d6; border-bottom: 1px solid #eaecef; padding-bottom: 0.3em; }\
	  h2 { border-bottom: 1px solid #eaecef; padding-bottom: 0.3em; color: #24292e; }\
	  code { background-color: rgba(27,31,35,0.05); padding: 0.2em 0.4em; border-radius: 3px; font-family: monospace; font-size: 85%; color: #d73a49; }\
	  pre { background-color: #f6f8fa; padding: 16px; border-radius: 6px; overflow: auto; border: 1px solid #dfe2e5; }\
	  blockquote { border-left: 4px solid #dfe2e5; color: #6a737d; padding-left: 16px; margin: 0; font-style: italic; }\
	  table { border-collapse: collapse; width: 100%; margin: 20px 0; }\
	  th, td { border: 1px solid #dfe2e5; padding: 8px 12px; }\
	  th { background-color: #f6f8fa; }\
	</style>\
	<div class="main-card">' > .tmp_header.html
	@echo '</div>' > .tmp_footer.html

	@# Run Pandoc
	pandoc $(INPUT) -s \
	  --metadata title=$(TITLE) \
	  -H .tmp_header.html \
	  -A .tmp_footer.html \
	  -o $(OUTPUT)

	@# Cleanup
	@rm $(TEMPLATES)
	@echo "Done! Open $(OUTPUT) to see the results."
