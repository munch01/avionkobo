#!/usr/bin/env python3
import json
import math
import os
import shutil
import subprocess
import sys
import time
from datetime import datetime
from pathlib import Path

import requests
from PIL import Image, ImageDraw, ImageFont


APP_ROOT = Path(__file__).resolve().parent
CONFIG_PATH = APP_ROOT / "config.json"
DEFAULT_CONFIG_PATH = APP_ROOT / "config.example.json"
OUTPUT_PATH = APP_ROOT / "output" / "kobo_sky.png"


def load_config():
    if CONFIG_PATH.exists():
        with CONFIG_PATH.open("r", encoding="utf-8") as f:
            return json.load(f)
    if DEFAULT_CONFIG_PATH.exists():
        with DEFAULT_CONFIG_PATH.open("r", encoding="utf-8") as f:
            return json.load(f)
    raise FileNotFoundError("Aucun fichier config.json ni config.example.json trouvé.")


CONFIG = load_config()
HOME = CONFIG["home"]
KOBO = CONFIG["kobo"]
OPEN_SKY = CONFIG["opensky"]
NETATMO = CONFIG["netatmo"]


def clamp(value, minimum, maximum):
    return max(minimum, min(maximum, value))


def haversine_km(lat1, lon1, lat2, lon2):
    r = 6371.0
    p1 = math.radians(lat1)
    p2 = math.radians(lat2)
    dlat = math.radians(lat2 - lat1)
    dlon = math.radians(lon2 - lon1)
    a = math.sin(dlat / 2) ** 2 + math.cos(p1) * math.cos(p2) * math.sin(dlon / 2) ** 2
    c = 2 * math.atan2(math.sqrt(a), math.sqrt(1 - a))
    return r * c


def safe_float(value, default=None):
    try:
        return float(value)
    except (TypeError, ValueError):
        return default


def safe_int(value, default=None):
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def choose_font(size, bold=False):
    candidates = [
        "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
        "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
        "/usr/share/fonts/ttf/dejavu/DejaVuSans-Bold.ttf",
        "/usr/share/fonts/ttf/dejavu/DejaVuSans.ttf",
        "DejaVuSans-Bold.ttf",
        "DejaVuSans.ttf",
    ]
    if bold:
        candidates = [p for p in candidates if "Bold" in p] + candidates
    for p in candidates:
        if os.path.exists(p):
            return ImageFont.truetype(p, size)
    return ImageFont.load_default()


def to_aircraft_type(typecode, model, manufacturer):
    if typecode:
        return typecode.strip().upper()
    if model:
        return model.strip().upper()
    if manufacturer:
        return manufacturer.strip().upper()
    return "UNKNOWN"


def open_sky_request(url, timeout=12):
    try:
        response = requests.get(url, timeout=timeout)
        response.raise_for_status()
        return response.json()
    except Exception:
        return {}


def get_nearest_flight():
    url = "https://opensky-network.org/api/states/all"
    data = open_sky_request(url)
    states = data.get("states", [])
    if not states:
        return None

    best = None
    best_dist = None
    radius_km = OPEN_SKY.get("radius_km", 80)

    for state in states:
        if len(state) < 17:
            continue

        icao24 = state[0]
        callsign = (state[1] or "").strip()
        longitude = safe_float(state[5])
        latitude = safe_float(state[6])
        altitude = safe_float(state[7])
        velocity = safe_float(state[9])
        heading = safe_float(state[10])
        on_ground = bool(state[8])

        if latitude is None or longitude is None or on_ground:
            continue

        distance = haversine_km(HOME["lat"], HOME["lon"], latitude, longitude)
        if distance > radius_km:
            continue

        if best_dist is None or distance < best_dist:
            metadata = fetch_aircraft_metadata(icao24)
            aircraft_type = to_aircraft_type(
                metadata.get("typecode"),
                metadata.get("model"),
                metadata.get("manufacturername"),
            )
            best = {
                "icao24": icao24,
                "callsign": callsign or "UNKNOWN",
                "latitude": latitude,
                "longitude": longitude,
                "altitude": altitude,
                "velocity": velocity,
                "heading": heading,
                "distance_km": distance,
                "aircraft_type": aircraft_type,
            }
            best_dist = distance

    return best


def fetch_aircraft_metadata(icao24):
    if not icao24:
        return {}
    url = f"https://opensky-network.org/api/metadata/aircraft/{icao24}"
    data = open_sky_request(url)
    if not isinstance(data, dict):
        return {}
    return {
        "typecode": data.get("typecode"),
        "model": data.get("model"),
        "manufacturername": data.get("manufacturername"),
    }


def netatmo_token():
    payload = {
        "grant_type": "password",
        "client_id": NETATMO["client_id"],
        "client_secret": NETATMO["client_secret"],
        "username": NETATMO["username"],
        "password": NETATMO["password"],
    }
    try:
        response = requests.post("https://api.netatmo.com/oauth2/token", data=payload, timeout=15)
        response.raise_for_status()
        data = response.json()
        return data.get("access_token")
    except Exception:
        return None


def get_nearest_netatmo_station():
    token = netatmo_token()
    if not token:
        return None

    try:
        headers = {"Authorization": f"Bearer {token}"}
        response = requests.get("https://api.netatmo.com/api/getstationsdata", headers=headers, timeout=15)
        response.raise_for_status()
        payload = response.json()
    except Exception:
        return None

    stations = payload.get("body", {}).get("devices", [])
    if not stations:
        return None

    best = None
    best_dist = None

    for station in stations:
        station_name = station.get("station_name") or station.get("module_name") or "Station"
        modules = station.get("modules", [])
        module_candidates = modules + [station]

        for module in module_candidates:
            dashboard = module.get("dashboard_data")
            if not dashboard:
                continue
            loc = module.get("place", {}).get("location") if isinstance(module.get("place"), dict) else None
            if not loc or len(loc) < 2:
                # some Netatmo modules store location on the station, fallback to station lat lon
                if station.get("place") and isinstance(station.get("place"), dict):
                    loc = station["place"].get("location")
                else:
                    continue

            lat = safe_float(loc[0])
            lon = safe_float(loc[1])
            if lat is None or lon is None:
                continue

            distance = haversine_km(HOME["lat"], HOME["lon"], lat, lon)
            weather = {
                "name": module.get("module_name") or station_name,
                "distance_km": distance,
                "temperature": safe_float(dashboard.get("Temperature")),
                "humidity": safe_float(dashboard.get("Humidity")),
                "pressure": safe_float(dashboard.get("Pressure")),
                "wind_strength": safe_float(dashboard.get("Windstrength")),
                "wind_angle": safe_float(dashboard.get("Windangle")),
                "rain": safe_float(dashboard.get("Rain")),
            }
            if best_dist is None or distance < best_dist:
                best = weather
                best_dist = distance

    return best


def render_plane(draw, width, height, y_offset=170):
    # Avion noir et blanc, en style vintage / Avioframe
    left = 40
    right = width - 35
    mid_y = y_offset

    # Clouds
    for cx, cy, r in [(90, 70, 38), (450, 90, 42), (200, 150, 46), (360, 150, 52)]:
        draw.ellipse((cx - r, cy - r, cx + r, cy + r), fill=245)

    # upper body
    draw.line((left, mid_y + 18, right - 40, mid_y + 18), fill=0, width=3)
    draw.line((left + 80, mid_y + 18, left + 120, mid_y + 2), fill=0, width=3)
    draw.line((left + 120, mid_y + 2, left + 240, mid_y + 2), fill=0, width=3)
    draw.line((left + 240, mid_y + 2, right - 60, mid_y + 18), fill=0, width=3)
    draw.line((right - 65, mid_y + 18, right, mid_y + 18), fill=0, width=3)

    # rear
    draw.line((left + 25, mid_y + 18, left + 42, mid_y + 50), fill=0, width=2)
    draw.line((right - 2, mid_y + 18, right + 20, mid_y + 50), fill=0, width=2)

    # wings and tail
    draw.line((left + 130, mid_y + 18, left + 160, mid_y - 35), fill=0, width=2)
    draw.line((left + 160, mid_y - 35, left + 240, mid_y + 18), fill=0, width=2)
    draw.line((left + 210, mid_y + 18, left + 210, mid_y - 50), fill=0, width=2)
    draw.line((right - 120, mid_y + 18, right - 85, mid_y - 26), fill=0, width=2)

    # nose detail
    draw.line((right - 52, mid_y + 18, right - 36, mid_y + 8), fill=0, width=2)


def render_screen(flight, weather):
    width = int(KOBO.get("width", 600))
    height = int(KOBO.get("height", 800))
    img = Image.new("L", (width, height), 255)
    draw = ImageDraw.Draw(img)

    title_font = choose_font(26, bold=True)
    big_font = choose_font(34, bold=True)
    medium_font = choose_font(22, bold=False)
    small_font = choose_font(17, bold=False)

    # Top sky area
    draw.rectangle((0, 0, width, 200), fill=220)
    draw.line((0, 200, width, 200), fill=0, width=2)

    # Title line
    draw.text((22, 18), "THE SKY OVER HOME", fill=0, font=title_font)
    draw.text((width - 150, 18), "LIVE", fill=0, font=title_font)
    draw.text((width - 145, 45), datetime.now().strftime("%H:%M"), fill=0, font=small_font)

    # aircraft
    render_plane(draw, width, height, y_offset=150)

    # body panel
    body_y = 310
    draw.rectangle((18, body_y, width - 18, 560), fill=235)
    draw.line((18, body_y, width - 18, body_y), fill=0, width=2)
    draw.line((18, 560, width - 18, 560), fill=0, width=2)

    # flight info
    if flight:
        callsign = (flight.get("callsign") or "UNKNOWN").replace(" ", "")
        aircraft_type = flight.get("aircraft_type", "UNKNOWN")
        altitude_ft = flight.get("altitude")
        speed_kmh = flight.get("velocity")
        dist = flight.get("distance_km")

        draw.text((36, 332), f"{aircraft_type} • {callsign}", fill=0, font=small_font)
        draw.text((36, 365), "MOST RECENT PASS", fill=0, font=small_font)
        draw.text((36, 395), f"HOME → {callsign}", fill=0, font=big_font)

        if altitude_ft is not None:
            alt_text = f"ALT {int(altitude_ft):,} FT"
        else:
            alt_text = "ALT N/A"
        if speed_kmh is not None:
            speed_text = f"SPD {int(speed_kmh):,} KM/H"
        else:
            speed_text = "SPD N/A"
        if dist is not None:
            dist_text = f"CLOSEST {dist:.1f} KM"
        else:
            dist_text = "CLOSEST N/A"

        draw.text((36, 450), alt_text, fill=0, font=medium_font)
        draw.text((36, 490), speed_text, fill=0, font=medium_font)
        draw.text((36, 530), dist_text, fill=0, font=medium_font)
    else:
        draw.text((36, 360), "NO FLIGHT DETECTED", fill=0, font=big_font)

    # weather panel
    weather_y = 575
    draw.rectangle((18, weather_y, width - 18, height - 18), fill=230)
    draw.line((18, weather_y, width - 18, weather_y), fill=0, width=2)

    if weather:
        station_name = weather.get("name") or "Nearest station"
        temp = weather.get("temperature")
        wind = weather.get("wind_strength")
        rain = weather.get("rain")
        humidity = weather.get("humidity")
        distance = weather.get("distance_km")

        draw.text((36, 600), station_name.upper(), fill=0, font=medium_font)
        draw.text((36, 635), f"TEMP: {temp:.1f}°C" if temp is not None else "TEMP: N/A", fill=0, font=medium_font)
        draw.text((36, 665), f"VENT: {wind:.0f} KM/H" if wind is not None else "VENT: N/A", fill=0, font=medium_font)
        draw.text((36, 695), f"PLUIE: {rain:.1f} MM" if rain is not None else "PLUIE: N/A", fill=0, font=medium_font)
        draw.text((36, 725), f"HUM: {humidity:.0f}%" if humidity is not None else "HUM: N/A", fill=0, font=medium_font)
        draw.text((300, 600), f"DIST: {distance:.1f} KM" if distance is not None else "DIST: N/A", fill=0, font=medium_font)
    else:
        draw.text((36, 620), "METEO UNAVAILABLE", fill=0, font=medium_font)

    OUTPUT_PATH.parent.mkdir(parents=True, exist_ok=True)
    img.save(str(OUTPUT_PATH))

    # Try to send to Kobo if available.
    if shutil.which("eips"):
        try:
            subprocess.run(["eips", "-f", "-g", str(OUTPUT_PATH)], check=False)
        except Exception:
            pass


def main():
    while True:
        flight = get_nearest_flight()
        weather = get_nearest_netatmo_station()
        render_screen(flight, weather)
        time.sleep(int(KOBO.get("refresh_seconds", 20)))


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("Arrêt du script.")
        sys.exit(0)
