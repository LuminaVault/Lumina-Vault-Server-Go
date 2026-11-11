#!/bin/bash
# youtube_content_dependencies.sh

# Detect and install dependencies for YouTube content tools

set -e

VENV_PRIORITY="/opt/hermes/.venv /opt/data/.venv /opt/data/home/.venv /opt/data/scoreboard-venv /opt/data/venv"

# Check if youtube-transcript-api is available in any virtual environment
check_youtube_transcript_api() {
    for venv in $VENV_PRIORITY; do
        if [ -f "$venv/bin/activate" ]; then
            source "$venv/bin/activate" 2>/dev/null
            if pip list 2>/dev/null | grep -q youtube-transcript-api; then
                echo "Found youtube-transcript-api in $venv"
                return 0
            fi
        fi
    done
    return 1
}

# Install youtube-transcript-api using the best available method
install_youtube_transcript_api() {
    # Try using uv if available
    if command -v uv >/dev/null; then
        echo "Using uv to install youtube-transcript-api..."
        uv pip install youtube-transcript-api
        return $?
    fi
    
    # Try using pip in the current environment
    if command -v pip >/dev/null; then
        echo "Using pip to install youtube-transcript-api..."
        pip install youtube-transcript-api
        return $?
    fi
    
    # Try using specific venv's pip
    for venv in $VENV_PRIORITY; do
        if [ -f "$venv/bin/pip" ]; then
            echo "Using $venv/bin/pip to install youtube-transcript-api..."
            "$venv/bin/pip" install youtube-transcript-api
            return $?
        fi
    done
    
    return 1
}

# Check if yt-dlp is available
check_yt_dlp() {
    if command -v yt-dlp >/dev/null; then
        return 0
    fi
    return 1
}

# Install yt-dlp
install_yt_dlp() {
    if command -v uv >/dev/null; then
        uv pip install yt-dlp
    elif command -v pip >/dev/null; then
        pip install yt-dlp
    else
        echo "ERROR: Neither uv nor pip is available to install yt-dlp"
        return 1
    fi
}

# Check if browser tools are available
check_browser_tools() {
    if command -v browser_navigate >/dev/null && command -v browser_vision >/dev/null; then
        return 0
    fi
    return 1
}

# Check if curl is available
check_curl() {
    if command -v curl >/dev/null; then
        return 0
    fi
    return 1
}

# Main entry point
main() {
    local action="$1"
    
    case $action in
        "check")
            echo "Checking dependencies..."
            check_youtube_transcript_api && exit 0 || echo "youtube-transcript-api not found"
            check_yt_dlp && exit 0 || echo "yt-dlp not found"
            check_browser_tools && exit 0 || echo "Browser tools not found"
            check_curl && exit 0 || echo "curl not found"
            ;;
        "install")
            echo "Attempting to install youtube-transcript-api..."
            install_youtube_transcript_api
            ;;
        "install-yt-dlp")
            echo "Attempting to install yt-dlp..."
            install_yt_dlp
            ;;
        "detect-method")
            echo "Detecting best transcript fetch method..."
            if check_youtube_transcript_api; then
                echo "Recommended: youtube-transcript-api"
            elif check_yt_dlp; then
                echo "Recommended: yt-dlp"
            elif check_browser_tools; then
                echo "Recommended: Browser extraction"
            elif check_curl; then
                echo "Recommended: API fallback"
            else
                echo "ERROR: No transcript fetch methods available"
            fi
            ;;
        *)
            echo "Usage: $0 {check|install|install-yt-dlp|detect-method}"
            exit 1
            ;;
    esac
}

main "$@"