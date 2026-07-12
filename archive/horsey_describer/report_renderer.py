"""
report_renderer.py
------------------
Converts a markdown field report into:
  - <timestamp>.md  (archived)
  - current_report.html  (overwritten each time, OBS points here)

Requires:
  pip install markdown
"""

import os
import time
import markdown

CSS_FILENAME  = "report.css"
HTML_FILENAME = "current_report.html"


def render_report(markdown_text: str, reports_dir: str = "./temp/reports") -> str:
    """
    Save markdown as <timestamp>.md and render current_report.html.
    Returns the path to current_report.html.
    """
    os.makedirs(reports_dir, exist_ok=True)
    timestamp = time.strftime("%Y%m%d_%H%M%S")

    # Archive the raw markdown
    md_path = os.path.join(reports_dir, f"{timestamp}.md")
    with open(md_path, "w", encoding="utf-8") as f:
        f.write(markdown_text)
    print(f"[Report] Markdown saved → {md_path}")

    # CSS sits alongside current_report.html in reports_dir
    css_abs = os.path.abspath(os.path.join(reports_dir, CSS_FILENAME))
    css_url = f"file://{css_abs}"

    # Convert to HTML
    body_html = markdown.markdown(markdown_text, extensions=["extra"])

    html = f"""<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>Field Report — {timestamp}</title>
    <link rel="stylesheet" href="{css_url}">
    <style>
        html, body {{
            margin: 0;
            padding: 20px 0;
            width: 100%;
            min-height: 100vh;
            display: flex;
            align-items: flex-start;
            justify-content: center;
            background: transparent;
        }}
    </style>
</head>
<body>
    <div class="page">
        <div class="report-header">
            <div class="classification">⚠ Classified — Cryptid Research Unit ⚠</div>
            <h1>Field Observation Report</h1>
            <div class="meta">Timestamp: {time.strftime("%Y-%m-%d %H:%M:%S")} &nbsp;|&nbsp; Status: ACTIVE INVESTIGATION</div>
        </div>
        <div class="report-body">
            {body_html}
        </div>
        <div class="report-footer">
            CRU Internal Document — Do Not Distribute — Destroy After Reading
        </div>
    </div>
</body>
</html>"""

    html_path = os.path.join(reports_dir, HTML_FILENAME)
    with open(html_path, "w", encoding="utf-8") as f:
        f.write(html)
    print(f"[Report] HTML rendered → {html_path}")

    return html_path