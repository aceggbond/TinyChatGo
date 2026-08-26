plugins { id("com.android.application") }

android {
    namespace = "com.aceggbond.tinychatgo"
    compileSdk = 35
    defaultConfig {
        applicationId = "com.aceggbond.tinychatgo"
        minSdk = 26
        targetSdk = 35
        versionCode = 110
        versionName = "1.1.0"
    }
    buildTypes { release { isMinifyEnabled = false; signingConfig = signingConfigs.getByName("debug") } }
}
