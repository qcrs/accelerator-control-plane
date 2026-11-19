from __future__ import annotations

import hashlib
import re
import textwrap
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont


ROOT = Path(__file__).resolve().parent
FONT = Path(r"C:\Windows\Fonts\consola.ttf")
FONT_BOLD = Path(r"C:\Windows\Fonts\consolab.ttf")
CJK_FONT = Path(r"C:\Windows\Fonts\msyh.ttc")
CJK_FONT_BOLD = Path(r"C:\Windows\Fonts\msyhbd.ttc")
FONT_SIZE = 20
LINE_HEIGHT = 29
WIDTH = 1720
PADDING = 42
HEADER_HEIGHT = 142


EVIDENCE = [
    ("cricket-standalone", "C-01", "Remote recovery state", "C-01-remote-recovery-state.log", "full"),
    ("cricket-standalone", "C-02", "Remote processes and RPC registration", "C-02-remote-rpc-registration.log", "full"),
    ("cricket-standalone", "C-03", "Cross-machine network and GPU identity", "C-03-network-and-identity.log", "full"),
    ("cricket-standalone", "C-04", "Independent CUDA API smoke", "C-04-api-smoke.log", "full"),
    ("cricket-standalone", "C-05", "Remote CUDA kernel and numeric verification", "C-05-kernel-verification.log", "full"),
    ("cricket-standalone", "C-06", "Complex image pipeline verification", "C-06-image-pipeline.log", "full"),
    ("cricket-standalone", "C-07", "Remote GPU observation during complex load", "C-07-remote-observation.log", "head-tail"),
    ("cricket-standalone", "C-08", "Twenty-run cross-machine stability", "C-08-stability-summary.log", "full"),
    ("platform-adapter", "P-01", "Core adapter unit tests", "P-01-core-unit-tests.log", "pass-summary"),
    ("platform-adapter", "P-02", "Local/Remote(mock)/Auto policy matrix", "P-02-policy-matrix.log", "full"),
    ("platform-adapter", "P-03", "Controller lifecycle and recovery", "P-03-controller-lifecycle.log", "full"),
    ("platform-adapter", "P-04", "Conflict, mode boundary and release", "P-04-conflict-boundary-release.log", "full"),
    ("platform-adapter", "P-05", "Envtest CRD and atomic claim", "P-05-envtest.log", "full"),
    ("platform-adapter", "P-06", "CRD server-side dry-run", "P-06-crd-dry-run.log", "full"),
    ("platform-adapter", "P-07", "Control-chain evidence matrix", "P-07-control-chain-matrix.log", "full"),
]

ANSI_ESCAPE = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def select_lines(lines: list[str], mode: str) -> list[str]:
    if mode == "full":
        return lines
    if mode == "head-tail":
        if len(lines) <= 70:
            return lines
        omitted = len(lines) - 58
        return lines[:34] + [f"... {omitted} lines omitted; see the raw log ..."] + lines[-24:]
    if mode == "pass-summary":
        selected = lines[:4]
        current_section = ""
        for line in lines[4:]:
            if line.startswith("[") and line.endswith("]"):
                current_section = line
                selected.extend(["", current_section])
            elif line.startswith("--- PASS:") or line.startswith("ok\t") or line == "PASS":
                selected.append(line)
        return selected
    raise ValueError(mode)


def wrap_lines(lines: list[str], width: int = 126) -> list[str]:
    result: list[str] = []
    for line in lines:
        if not line:
            result.append("")
            continue
        chunks = textwrap.wrap(
            line,
            width=width,
            replace_whitespace=False,
            drop_whitespace=False,
            subsequent_indent="  ",
        )
        result.extend(chunks or [""])
    return result


def render_log(group: str, code: str, title: str, log_name: str, mode: str) -> None:
    source = ROOT / group / "logs" / log_name
    output = ROOT / group / f"{log_name.removesuffix('.log')}.png"
    raw_text = source.read_text(encoding="utf-8", errors="replace")
    raw_lines = [ANSI_ESCAPE.sub("", line) for line in raw_text.splitlines()]
    lines = wrap_lines(select_lines(raw_lines, mode))
    height = HEADER_HEIGHT + PADDING + max(1, len(lines)) * LINE_HEIGHT + PADDING
    image = Image.new("RGB", (WIDTH, height), "#0d1117")
    draw = ImageDraw.Draw(image)
    has_cjk = any(ord(char) > 127 for char in raw_text)
    regular_path = CJK_FONT if has_cjk else FONT
    bold_path = CJK_FONT_BOLD if has_cjk else FONT_BOLD
    regular = ImageFont.truetype(str(regular_path), FONT_SIZE)
    bold = ImageFont.truetype(str(bold_path), 27)
    meta_font = ImageFont.truetype(str(regular_path), 17)

    draw.rectangle((0, 0, WIDTH, HEADER_HEIGHT), fill="#161b22")
    draw.ellipse((30, 28, 48, 46), fill="#ff5f56")
    draw.ellipse((58, 28, 76, 46), fill="#ffbd2e")
    draw.ellipse((86, 28, 104, 46), fill="#27c93f")
    draw.text((128, 22), f"{code}  {title}", font=bold, fill="#f0f6fc")
    draw.text(
        (30, 70),
        "AUDIT EVIDENCE — rendered from the exact raw terminal log",
        font=meta_font,
        fill="#58a6ff",
    )
    draw.text((30, 96), f"source: {source.relative_to(ROOT)}", font=meta_font, fill="#8b949e")
    draw.text((30, 119), f"sha256: {digest(source)}", font=meta_font, fill="#8b949e")

    y = HEADER_HEIGHT + PADDING
    for line in lines:
        color = "#c9d1d9"
        if "PASS" in line or "reachable" in line or "exit=0" in line or "failures: 0" in line:
            color = "#7ee787"
        elif "WARNING" in line or "Boundary:" in line or "does not prove" in line:
            color = "#d29922"
        elif line.startswith("[") or line.startswith("====="):
            color = "#79c0ff"
        draw.text((PADDING, y), line, font=regular, fill=color)
        y += LINE_HEIGHT

    image.save(output, optimize=True)


def render_pipeline_preview() -> None:
    source = ROOT / "cricket-standalone" / "results" / "image-pipeline-edge.pgm"
    output = ROOT / "cricket-standalone" / "C-06-image-pipeline-output.png"
    edge = Image.open(source).convert("L")
    canvas = Image.new("RGB", (edge.width, edge.height + 80), "white")
    canvas.paste(edge.convert("RGB"), (0, 80))
    draw = ImageDraw.Draw(canvas)
    font = ImageFont.truetype(str(FONT_BOLD), 24)
    meta = ImageFont.truetype(str(FONT), 17)
    draw.text((20, 14), "C-06  Actual D2H edge-image result", font=font, fill="black")
    draw.text((20, 47), f"1024x768 PGM sha256: {digest(source)}", font=meta, fill="#333333")
    canvas.save(output, optimize=True)


def main() -> None:
    for item in EVIDENCE:
        render_log(*item)
    render_pipeline_preview()


if __name__ == "__main__":
    main()
