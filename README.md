# Kobo Skyboard

Application Kobo pour afficher le vol le plus proche de chez toi et la météo la plus proche de la station Netatmo.

Caractéristiques :
- récupération des avions autour de ta position via OpenSky
- choix du vol le plus proche
- affichage noir/blanc inspiré d'Avioframe
- affichage des informations altitude, vitesse, type d'avion
- récupération de la station météo Netatmo la plus proche
- affichage température, vent, pluie, humidité
- rafraîchissement toutes les 20 secondes
- compatible avec NickelMenu

## Fichiers
- `skyboard.py` : cœur de l'application
- `launcher.sh` : script lancé depuis NickelMenu
- `config.example.json` : template de configuration
- `requirements.txt` : dépendances Python

## Installation locale

1. Copier le projet sur la Kobo dans `/mnt/onboard/.adds/kobo-skyboard`
2. Créer un fichier `config.json` à partir du template
3. Installer les dépendances :

```sh
python3 -m pip install -r requirements.txt
```

4. Rendre le launcher exécutable :

```sh
chmod +x /mnt/onboard/.adds/kobo-skyboard/launcher.sh
```

## Configuration

Copier le fichier de configuration :

```sh
cp config.example.json config.json
```

Puis modifier :
- `home.lat` et `home.lon` pour ta position
- `netatmo.client_id`, `netatmo.client_secret`, `netatmo.username`, `netatmo.password`

## Lancer depuis NickelMenu

Dans la config NickelMenu :

```txt
SkyBoard | /mnt/onboard/.adds/kobo-skyboard/launcher.sh
```

## Push GitHub

```bash
git init
git add .
git commit -m "Initial Kobo skyboard app"
git branch -M main
git remote add origin <URL_GITHUB>
git push -u origin main
```

## Notes

- Le type d'avion provient des métadonnées OpenSky si disponibles.
- Le script est conçu pour être robuste et afficher un PNG local en fallback si `eips` n'est pas présent.
- Sur une vraie Kobo, il faut vérifier que `python3`, `Pillow` et `eips` sont bien présents.
