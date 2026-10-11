# Kobo Skyboard

Application pour Kobo Aura N514 : affiche l'avion le plus proche, ses informations disponibles et la météo publique à la position de référence. L'écran est en niveaux de gris et se rafraîchit toutes les 20 secondes par défaut.

Les positions et types d'appareils proviennent d'ADSB.lol. La compagnie et l'itinéraire sont recherchés gratuitement auprès d'ADSBDB à partir de l'indicatif radio ; ces détails peuvent manquer pour certains vols. L'écran affiche compagnie, immatriculation et numéro de vol sur une ligne, puis le type d'appareil en dessous. L'altitude et la vitesse sont affichées en deux colonnes avec des pictogrammes et leurs valeurs, sans libellés. Si la recherche ne détecte temporairement aucun appareil, le dernier avion détecté reste affiché. Une photo de l'appareil exact est recherchée gratuitement sur Planespotters.net à partir de son immatriculation ; à défaut, Wikimedia Commons fournit une photo du modèle ou du constructeur quand elle existe. Le photographe est crédité sur l'image. Les conditions météo (température, humidité, précipitations et vent) proviennent d'Open-Meteo, sans compte ni clé API, sont mises en cache 10 minutes et affichées en quatre colonnes avec les pictogrammes et valeurs, sans libellé de lieu. L'image est dessinée directement sur l'écran de la liseuse par FBInk, sans navigateur ni ses barres de navigation.

Le programme principal est compilé en Go comme un binaire Linux ARMv7 statique. L'exécutable FBInk utilise les bibliothèques standard libc et libm du firmware Kobo. SkyBoard ne nécessite ni Python ni Pillow sur la liseuse.

## Fichiers

- `main.go` : récupération des données, dessin et affichage de l'écran
- `main_test.go` : tests unitaires
- `launcher.sh` : lance l'application et enregistre les erreurs dans `skyboard.log`
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

1. Copier `skyboard-arm`, `launcher.sh`, `cacert.pem`, `config.example.json` et un FBInk compilé avec la prise en charge des images dans `/mnt/onboard/.adds/kobo-skyboard/`. Le binaire se trouve dans `KoboRoot.tgz` de l'archive [KoboStuff](https://www.mobileread.com/forums/showthread.php?t=254214), sous `mnt/onboard/.niluje/usbnet/bin/fbink`. Extraire uniquement ce binaire ; ne pas installer toute l'archive KoboStuff. Le FBInk fourni par KOReader ne prend pas en charge l'affichage des images.
2. Créer `config.json` à partir de `config.example.json` et renseigner les coordonnées de référence.
3. Ajouter cette entrée à un fichier de configuration NickelMenu dans `/mnt/onboard/.adds/nm/` :

   ```txt
   menu_item :main :SkyBoard :cmd_spawn :quiet:/bin/sh /mnt/onboard/.adds/kobo-skyboard/launcher.sh
   ```

4. Éjecter proprement la liseuse et la redémarrer. Lancer ensuite **SkyBoard** depuis NickelMenu.

Le programme enregistre l'image dans `output/kobo_sky.png`, puis l'affiche directement sur la liseuse avec `fbink -f -i`. Si une ancienne configuration spécifie `eips` mais que cette commande est absente, SkyBoard bascule automatiquement sur FBInk placé dans son dossier. L'option `kobo.display_command` permet de choisir une autre commande d'affichage ; elle vaut `./fbink` par défaut. Le lien vers la page de la photo n'est plus cliquable dans ce mode natif. Les erreurs d'affichage et de réseau sont journalisées dans `skyboard.log`.

## Configuration

Copier le modèle puis modifier :

- `home.lat` et `home.lon` : coordonnées utilisées pour les avions et la météo publique
- `opensky.radius_km` : rayon de recherche des vols
- `kobo.refresh_seconds` : intervalle de rafraîchissement
- `kobo.display_command` : commande Kobo utilisée pour afficher l'image (`./fbink` par défaut ; FBInk doit être dans le dossier de SkyBoard)

Les anciennes clés `netatmo` éventuellement présentes dans `config.json` sont ignorées. Le fichier reste exclu de Git.
