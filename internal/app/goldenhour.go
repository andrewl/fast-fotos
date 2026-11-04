package app

import (
	"math"
	"time"
)

// Golden hour is when the sun is between these elevations (degrees above the horizon).
const (
	goldenHourMinElevation = -4.0
	goldenHourMaxElevation = 6.0
)

// sunElevation returns the sun's elevation in degrees at the given UTC instant and location,
// using the NOAA solar position approximation.
func sunElevation(utc time.Time, latitude, longitude float64) float64 {
	rad := math.Pi / 180
	jd := float64(utc.Unix())/86400 + 2440587.5
	t := (jd - 2451545.0) / 36525

	meanLong := math.Mod(280.46646+t*(36000.76983+t*0.0003032), 360)
	meanAnom := 357.52911 + t*(35999.05029-0.0001537*t)
	eccent := 0.016708634 - t*(0.000042037+0.0000001267*t)
	center := math.Sin(meanAnom*rad)*(1.914602-t*(0.004817+0.000014*t)) +
		math.Sin(2*meanAnom*rad)*(0.019993-0.000101*t) +
		math.Sin(3*meanAnom*rad)*0.000289
	trueLong := meanLong + center
	omega := 125.04 - 1934.136*t
	appLong := trueLong - 0.00569 - 0.00478*math.Sin(omega*rad)
	meanObliq := 23 + (26+(21.448-t*(46.815+t*(0.00059-t*0.001813)))/60)/60
	obliq := meanObliq + 0.00256*math.Cos(omega*rad)
	declination := math.Asin(math.Sin(obliq*rad)*math.Sin(appLong*rad)) / rad

	y := math.Pow(math.Tan(obliq*rad/2), 2)
	eqTime := 4 / rad * (y*math.Sin(2*meanLong*rad) - 2*eccent*math.Sin(meanAnom*rad) +
		4*eccent*y*math.Sin(meanAnom*rad)*math.Cos(2*meanLong*rad) -
		0.5*y*y*math.Sin(4*meanLong*rad) - 1.25*eccent*eccent*math.Sin(2*meanAnom*rad))

	minutes := float64(utc.Hour()*60+utc.Minute()) + float64(utc.Second())/60
	trueSolarTime := math.Mod(minutes+eqTime+4*longitude, 1440)
	hourAngle := trueSolarTime/4 - 180
	if trueSolarTime < 0 {
		hourAngle = trueSolarTime/4 + 180
	}

	cosZenith := math.Sin(latitude*rad)*math.Sin(declination*rad) +
		math.Cos(latitude*rad)*math.Cos(declination*rad)*math.Cos(hourAngle*rad)
	cosZenith = math.Max(-1, math.Min(1, cosZenith))
	return 90 - math.Acos(cosZenith)/rad
}

// isGoldenHour reports whether a photo was taken during the golden hour. takenAt holds the
// camera's local wall-clock time (EXIF carries no zone), so the UTC offset is approximated
// from longitude.
func isGoldenHour(takenAt time.Time, latitude, longitude float64) bool {
	offsetHours := math.Round(longitude / 15)
	local := time.Date(takenAt.Year(), takenAt.Month(), takenAt.Day(), takenAt.Hour(), takenAt.Minute(), takenAt.Second(), 0, time.UTC)
	utc := local.Add(-time.Duration(offsetHours * float64(time.Hour)))
	elevation := sunElevation(utc, latitude, longitude)
	return elevation >= goldenHourMinElevation && elevation <= goldenHourMaxElevation
}
