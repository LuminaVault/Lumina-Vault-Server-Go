---
name: knowledge-graph-animation
category: creative
description: Create animated graph visualizations from Obsidian vault data using HyperFrames
---
## Overview

This skill creates an animated graph visualization video from an Obsidian knowledge vault. It extracts link data from markdown files, creates an interactive D3.js graph, and renders it as an MP4 animation using HyperFrames.

## Prerequisites

- Obsidian vault with markdown files containing wikilinks
- HyperFrames installed (npm install -g hyperframes)
- Node.js ≥22 (required for HyperFrames)
- D3.js (v7)

## Steps

### 1. Extract Graph Data from Vault

```python
import json
import re
from pathlib import Path

def extract_links_from_file(filepath):
    content = Path(filepath).read_text()
    # Find wikilinks like [[Note Title]]
    links = re.findall(r'\[\[(.*?)\]\]', content)
    return links

def build_graph(vault_path):
    graph = {"nodes": [], "links": []}
    files = list(Path(vault_path).rglob("*.md"))
    
    for file in files:
        note_title = file.stem
        # Add node if not exists
        if note_title not in [n["id"] for n in graph["nodes"]]:
            graph["nodes"].append({"id": note_title, "group": 1})
        
        # Extract links
        linked_notes = extract_links_from_file(file)
        for target in linked_notes:
            if target in [n["id"] for n in graph["nodes"]]:
                graph["links"].append({"source": note_title, "target": target, "value": 1})
    
    return graph

# Example usage
vault_path = "/opt/data/obsidian-vault/Vault/wiki"
graph = build_graph(vault_path)
Path("/tmp/knowledge-graph.json").write_text(json.dumps(graph, indent=2))
```

### 2. Create Interactive HTML Visualization

Create `index.html` with D3.js force-directed graph:

```html
<!DOCTYPE html>
<html>
<head>
    <script src="https://d3js.org/d3.v7.min.js"></script>
    <style>
        body { margin: 0; overflow: hidden; background: #111; }
        .node { stroke: #fff; stroke-width: 0.5px; }
        .link { stroke: #999; stroke-opacity: 0.6; }
    </style>
</head>
<body>
    <script>
        const width = window.innerWidth;
        const height = window.innerHeight;
        
        const svg = d3.select("body").append("svg")
            .attr("width", width)
            .attr("height", height);
        
        d3.json("/tmp/knowledge-graph.json").then(data => {
            const simulation = d3.forceSimulation(data.nodes)
                .force("charge", d3.forceManyBody().strength(-300))
                .force("link", d3.forceLink(data.links).id(d => d.id).distance(100))
                .force("center", d3.forceCenter(width / 2, height / 2))
                .force("collide", d3.forceCollide().radius(20));
            
            const link = svg.append("g")
                .attr("class", "links")
                .selectAll("line")
                .data(data.links)
                .enter().append("line")
                .attr("stroke-width", d => Math.sqrt(d.value));
            
            const node = svg.append("g")
                .attr("class", "nodes")
                .selectAll("circle")
                .data(data.nodes)
                .enter().append("circle")
                .attr("r", 10)
                .attr("fill", d => d.group === 1 ? "#69b3a7" : "#404080")
                .call(d3.drag()
                    .on("start", dragstarted)
                    .on("drag", dragged));
            
            simulation.on("tick", () => {
                link
                    .attr("x1", d => d.source.x)
                    .attr("y1", d => d.source.y)
                    .attr("x2", d => d.target.x)
                    .attr("y2", d => d.target.y);
                
                node
                    .attr("cx", d => d.x)
                    .attr("cy", d => d.y);
            });
            
            function dragstarted(event) {
                if (!event.active) simulation.alphaTarget(0.3).restart();
                event.subject.fx = event.subject.x;
                event.subject.fy = event.subject.y;
            }
            
            function dragged(event) {
                event.subject.fx = event.x;
                event.subject.fy = event.y;
            }
        });
    </script>
</body>
</html>
```

### 3. Set Up HyperFrames Composition

Create `hyperframes.config.js`:

```javascript
const { HyperFrames } = require('hyperframes');

const config = {
  composition: {
    id: 'knowledge-graph',
    scenes: [
      {
        id: 'intro',
        duration: 2000,
        camera: { position: [0, 0, 1000] },
        elements: [
          { type: 'html', source: 'index.html', selector: 'body' }
        ]
      },
      {
        id: 'fly-through',
        duration: 10000,
        camera: { position: [500, 500, 1000], lookAt: [0, 0, 0] },
        elements: [
          { type: 'html', source: 'index.html', selector: 'body' }
        ]
      },
      {
        id: 'zoom-core',
        duration: 3000,
        camera: { position: [0, 0, 500] },
        elements: [
          { type: 'html', source: 'index.html', selector: 'body' }
        ]
      }
    ]
  },
  output: {
    path: '/tmp/knowledge-graph-final.mp4',
    format: 'mp4',
    quality: 'high'
  }
};

module.exports = config;
```

### 4. Render Animation

```bash
# Install HyperFrames if needed
npm install -g hyperframes

# Render the animation
hyperframes --config /path/to/hyperframes.config.js
```

### 5. Verify Output

Check the rendered video file:
```bash
ls -lh /tmp/knowledge-graph-final.mp4
```

## Troubleshooting

- **Lint errors**: HyperFrames may show lint warnings about missing composition ID or dimensions. These are often safe to ignore if rendering succeeds.
- **Node.js version**: HyperFrames requires Node.js ≥22. Check with `node --version`.
- **Performance**: For large graphs (>200 nodes), consider simplifying the visualization or increasing render quality.
- **Permission issues**: If the rendered file is owned by a different user (e.g., `hermes`), use `sudo cp` or change permissions with `chmod 644 /tmp/knowledge-graph-final.mp4`.
- **File transfer**: When copying from VPS to Mac, ensure you're running the command from the Mac, not the VPS. Use `rsync -avz <user>@<host>:/tmp/knowledge-graph-final.mp4 ~/Downloads/` from your Mac terminal.
- **SSH authentication**: If key-based auth fails, use password authentication. Default password for `hermes` user: `Hermes2024!`.
- **Docker containers**: If HyperFrames ran in Docker, copy the file out with `docker cp <container_id>:/tmp/knowledge-graph-final.mp4 /tmp/`.
- **File not found as root**: If root can't list the file due to ownership, use `cp /tmp/knowledge-graph-final.mp4 /tmp/knowledge-graph-final-root-copy.mp4` then transfer the copy.

## Notes

- The animation duration is determined by the sum of scene durations (15s total in example).
- Adjust camera positions and element selectors to customize the animation.
- For higher quality, set `quality: 'high'` in output configuration.