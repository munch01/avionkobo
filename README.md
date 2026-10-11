# Kobo Skyboard

Application pour Kobo Aura N514 : affiche l'avion le plus proche, ses informations disponibles et la météo publique à la position de référence. L'écran est en niveaux de gris et se rafraîchit toutes les 20 secondes par défaut.

Les positions et types d'appareils proviennent d'ADSB.lol. La compagnie et l'itinéraire sont recherchés gratuitement auprès d'ADSBDB à partir de l'indicatif radio ; ces détails peuvent manquer pour certains vols. Le tableau de bord affiche le numéro de vol et la compagnie sur une ligne, puis le type d'appareil en dessous. Si la recherche ne détecte temporairement aucun appareil, le dernier avion détecté reste affiché. Une photo de l'appareil exact est recherchée gratuitement sur Planespotters.net à partir de son immatriculation ; à défaut, Wikimedia Commons fournit une photo du modèle ou du constructeur quand elle existe. Le photographe est crédité et la photo Planespotters est cliquable pour ouvrir sa page d'origine. Les conditions météo (température, humidité, précipitations et vent) proviennent d'Open-Meteo, sans compte ni clé API, sont mises en cache 10 minutes et affichées en quatre colonnes avec des pictogrammes, sans libellé de lieu.

Le programme est compilé en Go comme un binaire Linux ARMv7 statique. Il ne nécessite ni Python, ni Pillow, ni bibliothèques système supplémentaires sur la liseuse.

## Fichiers

- `main.go` : récupération des données, dessin et affichage de l'écran
- `main_test.go` : tests unitaires
- `launcher.sh` : lance l'application et enregistre les erreurs dans `skyboard.log`
- `dashboard.html` : affiche et actualise l'image dans le navigateur intégré de la Kobo
- `config.example.json` : exemple de configuration sans identifiants
- `cacert.pem` : certificats racines Mozilla intégrés pour HTTPS sur les anciens firmwares

## Compiler et tester

Avec Go installé :

```sh
go test ./...
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o skyboard-arm .
```

Le binaire `skyboard-arm` doit être compilé pour ARMv7 ; un exécutable Windows ou Linux x86 ne fonctionnera pas sur la Kobo.

## Installation sur la Kobo

1. Copier `skyboard-arm`, `launcher.sh`, `dashboard.html`, `cacert.pem` et `config.example.json` dans `/mnt/onboard/.adds/kobo-skyboard/`.
2. Créer `config.json` à partir de `config.example.json` et renseigner les coordonnées de référence.
3. Ajouter cette entrée à un fichier de configuration NickelMenu dans `/mnt/onboard/.adds/nm/` :

   ```txt
   menu_item :main :SkyBoard :cmd_spawn :quiet:/bin/sh /mnt/onboard/.adds/kobo-skyboard/launcher.sh
     chain_success :nickel_browser :modal:http://127.0.0.1:8787/
   ```

4. Éjecter proprement la liseuse et la redémarrer. Lancer ensuite **SkyBoard** depuis NickelMenu. NickelMenu démarrera le service en arrière-plan et ouvrira le tableau de bord local dans le navigateur Kobo.

Le binaire sert le tableau de bord et son image en mémoire sur `127.0.0.1:8787`, puis le navigateur local actualise l'image périodiquement. Cela ne dépend pas de l'écriture d'un PNG sur le stockage Kobo. Les erreurs de démarrage et de réseau sont journalisées dans `skyboard.log`.

## Configuration

Copier le modèle puis modifier :

- `home.lat` et `home.lon` : coordonnées utilisées pour les avions et la météo publique
- `opensky.radius_km` : rayon de recherche des vols
- `kobo.refresh_seconds` : intervalle de rafraîchissement

Les anciennes clés `netatmo` éventuellement présentes dans `config.json` sont ignorées. Le fichier reste exclu de Git.
